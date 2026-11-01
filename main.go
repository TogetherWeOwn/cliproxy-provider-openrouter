package main

import (
	"context"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/TogetherWeOwn/cliproxy-provider-kit/cabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	provider     = "openrouter"
	version      = "0.2.1"
	modelPrefix  = "openrouter/"
	upstreamBase = "https://openrouter.ai/api/v1"
)

func main() {}
func init() {
	if err := cabi.Export(makePlugin); err != nil {
		panic("cannot install provider factory")
	}
}

// makePlugin wires one shared key-status reader into the spend guard, the
// executor and the quota card, so every path sees the same OpenRouter reading.
func makePlugin(host kit.Caller) (*kit.Runtime, error) {
	client := kit.NewQuotaHTTPClient(newQuotaTransport())
	keys := newKeyState(client)
	g := newGuard(keys)
	quota := newQuotaProvider(keys, g)
	keys.onInvalidate = quota.Invalidate
	models := newCatalog(g)
	auth := newAuthProvider(g)
	oauth, err := newPKCELogin(auth.ParseAuth, client)
	if err != nil {
		return nil, err
	}
	auth.oauth = oauth
	return kit.New(kit.Definition{
		Metadata:       pluginapi.Metadata{Name: provider, Version: version, Author: "TogetherWeOwn", GitHubRepository: "https://github.com/TogetherWeOwn/cliproxy-provider-openrouter"},
		ModelNamespace: provider,
		Configure:      configureBase,
		Capabilities: pluginapi.Capabilities{
			AuthProvider: auth, ModelRegistrar: models, ModelProvider: models, Executor: newExecutor(g, keys),
			ExecutorModelScope:   pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats: []string{"openai"}, ExecutorOutputFormats: []string{"openai"}, QuotaProvider: quota, ManagementAPI: oauth,
		},
		OnShutdown: func(context.Context) error { client.CloseIdleConnections(); return nil },
	}, host)
}
