package main

import (
	"strings"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// serveID maps an upstream model id to the id this plugin serves. The kit
// only serves ids carrying "openrouter/" exactly once, with no exception for
// first-party models — so OpenRouter's own "openrouter/…" ids (Auto Router,
// free variants) pass through unchanged, while every other upstream id is
// prefixed once. The kit conformance check is the arbiter: whatever it
// accepts here is what the runtime serves.
func serveID(upstream string) string {
	if kit.CheckModelNamespace(provider, []pluginapi.ModelInfo{{ID: upstream}}) == nil {
		return upstream
	}
	return modelPrefix + upstream
}

// upstreamID inverts serveID for dispatch. A served id that only gained its
// namespace from the prefix maps back by trimming once; a served id that
// already carried it (first-party "openrouter/…" ids, which have no second
// segment after the prefix) maps to itself. Nested first-party ids
// ("openrouter/a/b" upstream) do not exist; if one ever appears the trim
// branch misroutes it to "a/b" and the upstream answers 404, fail-closed.
func upstreamID(served string) (string, bool) {
	rest, ok := strings.CutPrefix(served, modelPrefix)
	if !ok || rest == "" {
		return "", false
	}
	if strings.Contains(rest, "/") {
		if serveID(rest) != served {
			return "", false
		}
		return rest, true
	}
	if serveID(served) != served {
		return "", false
	}
	return served, true
}
