package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Both GET /key and GET /credits (both with the regular key) follow the
// host's proxy policy: the auth's proxy_url, else the host's proxy-url.
func TestQuotaPollUsesAuthOrHostProxy(t *testing.T) {
	recorder := func() (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var seen []string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.String())
			mu.Unlock()
			if strings.HasSuffix(r.URL.Path, "/credits") {
				_, _ = w.Write(fixture(t, "credits_ok.json"))
				return
			}
			_, _ = w.Write(fixture(t, "key_monthly_limit.json"))
		}))
		t.Cleanup(s.Close)
		return s, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
	}
	authProxy, authSeen := recorder()
	hostProxy, hostSeen := recorder()
	client := kit.NewQuotaHTTPClient(newQuotaTransport())
	defer client.CloseIdleConnections()
	keys := newKeyState(client)
	keys.base = "http://openrouter.invalid/api/v1"
	q := newQuotaProvider(keys, newGuard(keys))
	host := pluginapi.HostConfigSummary{ProxyURL: hostProxy.URL}
	for _, r := range []pluginapi.QuotaFetchRequest{
		{AuthID: "a", StorageJSON: []byte(`{"api_key":"one"}`), Metadata: map[string]any{"proxy_url": authProxy.URL}, Host: host},
		{AuthID: "b", StorageJSON: []byte(`{"api_key":"two"}`), Host: host},
	} {
		out, err := q.FetchQuota(context.Background(), r)
		if err != nil || len(out.Groups) != 1 {
			t.Fatalf("%s: %+v %v", r.AuthID, out, err)
		}
	}
	// /key and /credits are read concurrently, so their order is not fixed.
	want := []string{"http://openrouter.invalid/api/v1/credits", "http://openrouter.invalid/api/v1/key"}
	for name, got := range map[string][]string{"auth": authSeen(), "host": hostSeen()} {
		sort.Strings(got)
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s proxy saw %v", name, got)
		}
	}
}

// HTTP/2 connections are reused by destination before the proxy is chosen, so
// a transport shared across auths must not speak HTTP/2.
func TestQuotaTransportDoesNotShareConnectionsAcrossProxies(t *testing.T) {
	transport := newQuotaTransport()
	if transport.Proxy == nil || transport.Protocols == nil || transport.Protocols.HTTP2() || !transport.Protocols.HTTP1() || transport.ForceAttemptHTTP2 {
		t.Fatal("quota transport must be HTTP/1.1-only with a proxy function")
	}
}

func TestInvalidProxyFailsClosed(t *testing.T) {
	client := kit.NewQuotaHTTPClient(newQuotaTransport())
	defer client.CloseIdleConnections()
	keys := newKeyState(client)
	keys.base = "http://openrouter.invalid/api/v1"
	g := newGuard(keys)
	g.observeHost(pluginapi.HostConfigSummary{})
	err := g.admit(context.Background(), "auth-1", credentials{apiKey: exampleKey}, []byte(`{"api_key":"example-key","proxy_url":"ftp://bad"}`), nil, "a/b")
	if e := rpcErr(t, err); e.Code != "quota_unverified" {
		t.Fatalf("%+v", e)
	}
}
