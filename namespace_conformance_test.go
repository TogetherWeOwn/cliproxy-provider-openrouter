package main

import (
	"context"
	"testing"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Every model id this plugin can emit (registration, static fallback and
// per-auth discovery) must carry the fixed "openrouter/" namespace exactly
// once, enforced by the kit at runtime.
func TestNamespaceConformance(t *testing.T) {
	host := &fakeHost{body: fixture(t, "models.json")}
	cat := newTestCatalog(newClock(now0))
	reg, err := cat.RegisterModels(context.Background(), pluginapi.ModelRegistrationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	static, err := cat.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := cat.ModelsForAuth(context.Background(), modelRequest(host))
	if err != nil {
		t.Fatal(err)
	}
	models := append(append(append([]pluginapi.ModelInfo{}, reg.Models...), static.Models...), discovered.Models...)
	kit.AssertModelNamespace(t, provider, models)
}

// serveID prefixes once, except ids already carrying the namespace (including
// OpenRouter's own first-party ids); upstreamID inverts it, and doubled ids
// never round-trip.
func TestServeUpstreamIDRoundTrip(t *testing.T) {
	for upstream, served := range map[string]string{
		"anthropic/claude-sonnet-4.5":            "openrouter/anthropic/claude-sonnet-4.5",
		"meta-llama/llama-3.3-70b-instruct:free": "openrouter/meta-llama/llama-3.3-70b-instruct:free",
		"~anthropic/claude-sonnet-latest":        "openrouter/~anthropic/claude-sonnet-latest",
		"openrouter/auto":                        "openrouter/auto",
		"openrouter/free":                        "openrouter/free",
	} {
		if got := serveID(upstream); got != served {
			t.Fatalf("serveID(%q) = %q, want %q", upstream, got, served)
		}
		if got, ok := upstreamID(served); !ok || got != upstream {
			t.Fatalf("upstreamID(%q) = %q, %v; want %q, true", served, got, ok, upstream)
		}
	}
	// Charset/length validation stays in buildRequest (validUpstreamModel);
	// upstreamID only inverts the namespace mapping.
	for _, bad := range []string{"", "openrouter/", "openrouter", "anthropic/claude", "openrouter/openrouter/auto"} {
		if got, ok := upstreamID(bad); ok {
			t.Fatalf("upstreamID(%q) = %q, true; want rejected", bad, got)
		}
	}
}
