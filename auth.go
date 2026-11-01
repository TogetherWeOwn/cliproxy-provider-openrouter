package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const maxReserveUSD = 1_000_000

// credentials are the plugin-owned auth fields. Keys never appear in errors.
// The plugin uses only the regular OpenRouter API key: it reads GET /key and
// GET /credits and runs inference with it.
type credentials struct {
	apiKey         string
	reserve        float64 // min_remaining_usd
	allowUnlimited bool    // allow_unlimited_key
}

// digest identifies the credential version for caches keyed by auth ID.
func (c credentials) digest() [32]byte {
	return sha256.Sum256([]byte(c.apiKey))
}

func invalidAuth(message string) *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_auth", Message: "invalid_auth: " + message, HTTPStatus: http.StatusBadRequest}
}

// ownFields are the plugin's fields; hostFields are owned by the host and
// may be written into an auth file by CLIProxyAPI v8.0.49 itself (the kit
// fields, plus keys the management API patches into credential metadata).
// Any other top-level field is rejected, so a typo such as "min_remaining"
// cannot silently drop the spend reserve.
var (
	ownFields  = map[string]bool{"type": true, "api_key": true, "min_remaining_usd": true, "allow_unlimited_key": true}
	hostFields = map[string]bool{"label": true, "prefix": true, "proxy_url": true, "weight": true, "priority": true, "disabled": true, "note": true, "headers": true, "request_retry": true, "disable_cooling": true, "excluded_models": true, "excluded_models_hash": true, "websockets": true}
)

// validKey accepts 1-256 printable ASCII characters without spaces. The
// sk-or- prefix is not required: OpenRouter does not document the key shape
// as a contract.
func validKey(key string) bool {
	if len(key) == 0 || len(key) > 256 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] > '~' {
			return false
		}
	}
	return true
}

func numberValue(v any) (float64, bool) {
	var n float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if n, err = x.Float64(); err != nil {
			return 0, false
		}
	case float64:
		n = x
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

// credentialsFields validates the plugin fields of an already merged auth.
func credentialsFields(fields map[string]any) (credentials, error) {
	var c credentials
	key, _ := fields["api_key"].(string)
	c.apiKey = strings.TrimSpace(key)
	if !validKey(c.apiKey) {
		return c, invalidAuth("api_key must be 1-256 printable ASCII characters without spaces")
	}
	if v, ok := fields["min_remaining_usd"]; ok {
		n, isNumber := numberValue(v)
		if !isNumber || n < 0 || n > maxReserveUSD {
			return c, invalidAuth("min_remaining_usd must be a number from 0 to 1000000")
		}
		c.reserve = n
	}
	if v, ok := fields["allow_unlimited_key"]; ok {
		b, isBool := v.(bool)
		if !isBool {
			return c, invalidAuth("allow_unlimited_key must be a boolean")
		}
		c.allowUnlimited = b
	}
	return c, nil
}

// credentialsFrom validates at every use, including host metadata overlays.
func credentialsFrom(storage []byte, metadata map[string]any) (credentials, error) {
	fields, err := kit.AuthFields(storage, metadata)
	if err != nil {
		return credentials{}, err
	}
	return credentialsFields(fields)
}

// authProvider adds plugin-field validation and digest-only duplicate
// detection to the kit API-key provider. Ownership is by file name, so a
// watcher's repeated parse of one file is safe. A key whose owning file no
// longer exists (moved or renamed) passes to the next file that holds it.
type authProvider struct {
	kit.APIKeyAuth
	guard  *guard
	oauth  *pkceLogin
	mu     sync.Mutex
	files  map[string][32]byte
	owners map[[32]byte]owner
}

// StartLogin runs the real PKCE login (see pkce.go), shadowing the embedded
// kit API-key login. API-key auths keep working through uploaded auth files.
func (p *authProvider) StartLogin(ctx context.Context, req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	if p == nil || p.oauth == nil {
		return pluginapi.AuthLoginStartResponse{}, &pluginabi.Error{Code: "login_unavailable", Message: "OAuth login is not configured", HTTPStatus: 500}
	}
	return p.oauth.StartLogin(ctx, req)
}

// PollLogin completes the PKCE login, shadowing the embedded kit poller.
func (p *authProvider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	if p == nil || p.oauth == nil {
		return pluginapi.AuthLoginPollResponse{}, &pluginabi.Error{Code: "login_unavailable", Message: "OAuth login is not configured", HTTPStatus: 500}
	}
	return p.oauth.PollLogin(ctx, req)
}

// owner is the auth file that holds a key: its name and, when the host
// reports it, its path.
type owner struct{ file, path string }

// gone reports that the owning file was removed, so its key is free.
func (o owner) gone() bool {
	if o.path == "" {
		return false
	}
	_, err := os.Stat(o.path)
	return errors.Is(err, fs.ErrNotExist)
}

func newAuthProvider(g *guard) *authProvider {
	return &authProvider{guard: g, APIKeyAuth: kit.APIKeyAuth{Provider: provider, Validate: func(fields map[string]any) error {
		if _, ok := fields["id"]; ok {
			return invalidAuth("auth identity is derived from the API key")
		}
		if _, ok := fields["management_api_key"]; ok {
			return invalidAuth("management_api_key is not supported; the plugin uses only the regular api_key")
		}
		for name := range fields {
			if !ownFields[name] && !hostFields[name] {
				return invalidAuth("unknown auth field; expected api_key, min_remaining_usd, allow_unlimited_key and host fields")
			}
		}
		c, err := credentialsFields(fields)
		if err == nil {
			fields["api_key"] = c.apiKey
		}
		return err
	}}, files: make(map[string][32]byte), owners: make(map[[32]byte]owner)}
}

func (p *authProvider) ParseAuth(ctx context.Context, r pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	p.guard.observeHost(r.Host)
	out, err := p.APIKeyAuth.ParseAuth(ctx, r)
	if err != nil || !out.Handled {
		return out, err
	}
	c, err := credentialsFrom(out.Auth.StorageJSON, out.Auth.Metadata)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	digest := c.digest()
	p.mu.Lock()
	defer p.mu.Unlock()
	if o, ok := p.owners[digest]; ok && o.file != r.FileName {
		if !o.gone() {
			return pluginapi.AuthParseResponse{}, &pluginabi.Error{Code: "duplicate_auth", Message: "duplicate_auth: the same API key is already configured in another auth file", HTTPStatus: http.StatusBadRequest}
		}
		delete(p.files, o.file)
	}
	if old, ok := p.files[r.FileName]; ok && old != digest {
		delete(p.owners, old)
	}
	p.files[r.FileName], p.owners[digest] = digest, owner{file: r.FileName, path: r.Path}
	return out, nil
}

func (p *authProvider) RefreshAuth(ctx context.Context, r pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	p.guard.observeHost(r.Host)
	if _, err := credentialsFrom(r.StorageJSON, r.Metadata); err != nil {
		return pluginapi.AuthRefreshResponse{}, err
	}
	return p.APIKeyAuth.RefreshAuth(ctx, r)
}
