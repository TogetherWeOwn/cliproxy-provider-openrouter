package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func parse(p *authProvider, file, raw string) (pluginapi.AuthParseResponse, error) {
	return p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{FileName: file, RawJSON: []byte(raw)})
}

func TestParseAuthAcceptsDocumentedSchema(t *testing.T) {
	p := newAuthProvider(newGuard(nil))
	out, err := parse(p, "a.json", `{"type":"openrouter","api_key":"  example-key  ","management_api_key":"example-management-key","min_remaining_usd":0.5,"allow_unlimited_key":false,"label":"Example","prefix":"acct","proxy_url":"direct","weight":2,"priority":1,"disabled":false,"note":"host field"}`)
	if err != nil || !out.Handled {
		t.Fatalf("%v", err)
	}
	digest := sha256.Sum256([]byte(exampleKey))
	if out.Auth.ID != "openrouter:"+hex.EncodeToString(digest[:6]) {
		t.Fatalf("id %q", out.Auth.ID)
	}
	if out.Auth.Prefix != "acct" || out.Auth.Label != "Example" || out.Auth.ProxyURL != "direct" {
		t.Fatalf("host fields %+v", out.Auth)
	}
	c, err := credentialsFrom(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil || c.apiKey != exampleKey || c.managementKey != "example-management-key" || c.reserve != 0.5 || c.allowUnlimited {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestParseAuthIgnoresOtherProviders(t *testing.T) {
	p := newAuthProvider(newGuard(nil))
	for _, raw := range []string{`{"type":"synthetic","api_key":"example-key"}`, `{"type":"OpenRouter","api_key":"example-key"}`, `{"api_key":"example-key"}`} {
		if out, err := parse(p, "x.json", raw); err != nil || out.Handled {
			t.Fatalf("%s handled=%v err=%v", raw, out.Handled, err)
		}
	}
}

func TestParseAuthRejections(t *testing.T) {
	long := strings.Repeat("k", 257)
	for _, raw := range []string{
		`{"type":"openrouter","api_key":"example-key","id":"chosen"}`,
		`{"type":"openrouter","api_key":"example-key","min_remaining":5}`,
		`{"type":"openrouter","api_key":"example-key","base_url":"https://elsewhere.example"}`,
		`{"type":"openrouter","api_key":"example key"}`,
		`{"type":"openrouter","api_key":"example\tkey"}`,
		`{"type":"openrouter","api_key":"   "}`,
		`{"type":"openrouter","api_key":"` + long + `"}`,
		`{"type":"openrouter","api_key":"exämple"}`,
		`{"type":"openrouter","api_key":"example-key","management_api_key":"example-key"}`,
		`{"type":"openrouter","api_key":"example-key","management_api_key":""}`,
		`{"type":"openrouter","api_key":"example-key","management_api_key":5}`,
		`{"type":"openrouter","api_key":"example-key","min_remaining_usd":-1}`,
		`{"type":"openrouter","api_key":"example-key","min_remaining_usd":"5"}`,
		`{"type":"openrouter","api_key":"example-key","min_remaining_usd":1e7}`,
		`{"type":"openrouter","api_key":"example-key","allow_unlimited_key":"true"}`,
	} {
		_, err := parse(newAuthProvider(newGuard(nil)), "x.json", raw)
		if err == nil {
			t.Fatalf("accepted %s", raw)
		}
		if e := rpcErr(t, err); e.HTTPStatus != 400 || strings.Contains(e.Message, "example-key") || strings.Contains(e.Message, long) {
			t.Fatalf("%s: %+v", raw, e)
		}
	}
}

func TestParseAuthDuplicateKeyAcrossFiles(t *testing.T) {
	p := newAuthProvider(newGuard(nil))
	raw := `{"type":"openrouter","api_key":"example-key"}`
	if _, err := parse(p, "a.json", raw); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(p, "a.json", raw); err != nil {
		t.Fatalf("re-parsing the same file must succeed: %v", err)
	}
	_, err := parse(p, "b.json", ` {"type":"openrouter","api_key":" example-key"}`)
	if e := rpcErr(t, err); e.Code != "duplicate_auth" || strings.Contains(e.Message, exampleKey) {
		t.Fatalf("%+v", e)
	}
	// Once a.json moves to another key, its old key may be used elsewhere.
	if _, err := parse(p, "a.json", `{"type":"openrouter","api_key":"rotated-key"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(p, "b.json", raw); err != nil {
		t.Fatalf("released key still owned: %v", err)
	}
}

func TestParseAuthRecordsHostProxy(t *testing.T) {
	g := newGuard(nil)
	p := newAuthProvider(g)
	_, _ = p.ParseAuth(context.Background(), pluginapi.AuthParseRequest{FileName: "a.json", RawJSON: []byte(`{"type":"openrouter","api_key":"example-key"}`), Host: pluginapi.HostConfigSummary{ProxyURL: "socks5://proxy.invalid:1080"}})
	if v := g.hostProxy.Load(); v == nil || *v != "socks5://proxy.invalid:1080" {
		t.Fatal("parse did not record the host proxy")
	}
	_, _ = p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "x", StorageJSON: storage(""), Host: pluginapi.HostConfigSummary{ProxyURL: "direct"}})
	if v := g.hostProxy.Load(); v == nil || *v != "direct" {
		t.Fatal("refresh did not record the host proxy")
	}
}

func TestRefreshPreservesStorageAndValidates(t *testing.T) {
	p := newAuthProvider(newGuard(nil))
	raw := []byte(`{"api_key":"example-key","min_remaining_usd":2,"allow_unlimited_key":true}`)
	meta := map[string]any{"type": provider, "label": "Example", "prefix": "acct", "note": "host"}
	out, err := p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "openrouter:abc", StorageJSON: raw, Metadata: meta})
	if err != nil || !bytes.Equal(out.Auth.StorageJSON, raw) || out.Auth.ID != "openrouter:abc" || out.Auth.Label != "Example" || out.Auth.Prefix != "acct" {
		t.Fatalf("%+v %v", out.Auth, err)
	}
	if _, err = p.RefreshAuth(context.Background(), pluginapi.AuthRefreshRequest{AuthID: "x", StorageJSON: []byte(`{"api_key":"example-key","min_remaining_usd":-3}`)}); err == nil {
		t.Fatal("refresh accepted an invalid reserve")
	}
}

func TestCredentialsUseMetadataOverlay(t *testing.T) {
	c, err := credentialsFrom(storage(`,"min_remaining_usd":1`), map[string]any{"min_remaining_usd": 2.5, "allow_unlimited_key": true})
	if err != nil || c.reserve != 2.5 || !c.allowUnlimited {
		t.Fatalf("%+v %v", c, err)
	}
}
