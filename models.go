package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	catalogTTL       = 10 * time.Minute
	maxModelID       = 128
	maxDescription   = 300
	displayNameAffix = " via OpenRouter"
)

// One "/" after an optional "~" alias marker; variant suffixes such as
// ":free" are allowed in the model part.
var modelIDPattern = regexp.MustCompile(`^~?[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._~:-]*$`)

// catalog serves models through the kit ModelCatalog (v0.5.0).
//
// The kit starts no goroutine and fetches foreground-only inside ModelsForAuth:
// every call attempts a fresh fetch (the plugin's old 10-minute TTL skip gate
// is gone — the TTL now bounds only the kit's freshness markers), and results
// fall back fresh → durable last-good → static. Static registration stays
// empty, so a failed fetch with no last-good surfaces the fetch error; there
// is no background refresh or Timeline-B recovery (recovery waits for the
// next host event).
//
// The plugin keeps its own Fetch because OpenAIModelsFetcher parses only
// id/object/created/owned_by and would drop the OpenRouter-specific policy
// and enrichment (output-modalities, variable-price, expiration, :batch,
// display names, thinking). The Fetch below derives the bearer header from
// the request exactly as before (the kit never derives credentials) and
// returns upstream pre-namespace ids, with Name already carrying the served
// namespace so the kit's prefixing leaves the name untouched. The plugin has
// no static list, overrides or allow/deny globs — its keepModel policy plays
// that role, and an all-filtered refresh is therefore an empty fetch that
// keeps last-good.
type catalog struct {
	inner *kit.ModelCatalog
	guard *guard
	// base overrides the upstream base URL; tests point it at loopback.
	base string
	now  func() time.Time
}

func newCatalog(g *guard) *catalog {
	cat := &catalog{guard: g, now: time.Now}
	cat.inner = kit.NewModelCatalogWithTTL(kit.ModelCatalogConfig{
		Namespace: provider,
		Fetch:     cat.fetch,
	}, catalogTTL)
	return cat
}

func (cat *catalog) RegisterModels(ctx context.Context, req pluginapi.ModelRegistrationRequest) (pluginapi.ModelRegistrationResponse, error) {
	resp, err := cat.inner.RegisterModels(ctx, req)
	if err == nil {
		resp.Provider = provider
	}
	return resp, err
}

func (cat *catalog) StaticModels(ctx context.Context, req pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	cat.guard.observeHost(req.Host)
	resp, err := cat.inner.StaticModels(ctx, req)
	if err == nil {
		resp.Provider = provider
	}
	return resp, err
}

// ModelsForAuth lists models through the host HTTP client, so proxy and
// request logging stay host-owned. No model is probed: probes cost money.
func (cat *catalog) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	resp, err := cat.inner.ModelsForAuth(ctx, req)
	if err == nil {
		resp.Provider = provider
	}
	return resp, err
}

func (cat *catalog) endpoint() string {
	if cat.base != "" {
		return cat.base
	}
	return selectedBase(upstreamBase)
}

func (cat *catalog) fetch(ctx context.Context, r pluginapi.AuthModelRequest) ([]pluginapi.ModelInfo, error) {
	cat.guard.observeHost(r.Host)
	c, err := credentialsFrom(r.StorageJSON, r.Metadata)
	if err != nil {
		return nil, err
	}
	if r.HTTPClient == nil {
		return nil, &pluginabi.Error{Code: "host_unavailable", Message: "host_unavailable: model discovery requires the host HTTP client", HTTPStatus: http.StatusServiceUnavailable}
	}
	resp, err := r.HTTPClient.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: cat.endpoint() + "/models", Headers: http.Header{"Authorization": {"Bearer " + c.apiKey}, "Accept": {"application/json"}}})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e, _ := classifyFailure(resp, cat.now(), nil, "")
		return nil, e
	}
	if len(resp.Body) > kit.MaxModelsBody {
		return nil, invalidModels()
	}
	return parseModels(resp.Body, cat.now())
}

func invalidModels() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_models", Message: "invalid_models: OpenRouter returned an invalid model catalog", HTTPStatus: http.StatusBadGateway}
}

type modelWire struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Created       int64   `json:"created"`
	Description   string  `json:"description"`
	ContextLength *int64  `json:"context_length"`
	Expiration    *string `json:"expiration_date"`
	Architecture  *struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	Pricing *struct {
		Prompt     json.RawMessage `json:"prompt"`
		Completion json.RawMessage `json:"completion"`
	} `json:"pricing"`
	TopProvider *struct {
		MaxCompletionTokens *int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	SupportedParameters []string `json:"supported_parameters"`
	Reasoning           *struct {
		SupportedEfforts []string `json:"supported_efforts"`
		Mandatory        bool     `json:"mandatory"`
	} `json:"reasoning"`
}

// variablePrice reports a negative price ("-1"): routers whose cost is
// unknowable before the call.
func variablePrice(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return strings.HasPrefix(strings.TrimSpace(x), "-")
	case float64:
		return x < 0
	}
	return false
}

func expired(raw *string, now time.Time) bool {
	if raw == nil || len(*raw) < 10 {
		return false
	}
	day, err := time.Parse("2006-01-02", (*raw)[:10])
	if err != nil {
		return false
	}
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return !day.After(today)
}

func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// keepModel applies the registration filters.
func keepModel(m modelWire, now time.Time) bool {
	switch {
	case m.ID == "" || len(m.ID) > maxModelID || !modelIDPattern.MatchString(m.ID):
		return false
	case strings.HasSuffix(m.ID, ":batch"): // Batch API only
		return false
	case m.Architecture != nil && m.Architecture.OutputModalities != nil && !contains(m.Architecture.OutputModalities, "text"):
		return false
	case m.Pricing != nil && (variablePrice(m.Pricing.Prompt) || variablePrice(m.Pricing.Completion)):
		return false
	case expired(m.Expiration, now):
		return false
	}
	return true
}

// modelInfo maps one upstream entry to its pre-namespace catalog record:
// ID stays upstream while Name carries the served namespace, so the kit's
// prefixing produces the exact served id and the host lists the same value
// as the Gemini model name.
func modelInfo(m modelWire) pluginapi.ModelInfo {
	name := m.Name
	if strings.TrimSpace(name) == "" {
		name = m.ID
	}
	info := pluginapi.ModelInfo{
		ID: m.ID, Object: "model", OwnedBy: provider, Name: serveID(m.ID), Created: m.Created,
		DisplayName: name + displayNameAffix, Description: truncateRunes(m.Description, maxDescription),
		SupportedParameters: m.SupportedParameters,
	}
	if m.ContextLength != nil {
		info.ContextLength = *m.ContextLength
	}
	if m.TopProvider != nil && m.TopProvider.MaxCompletionTokens != nil {
		info.MaxCompletionTokens = *m.TopProvider.MaxCompletionTokens
		info.OutputTokenLimit = *m.TopProvider.MaxCompletionTokens
	}
	if m.Architecture != nil {
		info.SupportedInputModalities = m.Architecture.InputModalities
		info.SupportedOutputModalities = m.Architecture.OutputModalities
	}
	if m.Reasoning != nil && len(m.Reasoning.SupportedEfforts) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: m.Reasoning.SupportedEfforts, ZeroAllowed: !m.Reasoning.Mandatory}
	}
	return info
}

// parseModels decodes each entry on its own, so one malformed entry is
// dropped instead of failing the catalog.
func parseModels(body []byte, now time.Time) ([]pluginapi.ModelInfo, error) {
	var wire struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Data == nil {
		return nil, invalidModels()
	}
	out := []pluginapi.ModelInfo{}
	seen := make(map[string]bool)
	for _, raw := range wire.Data {
		var m modelWire
		if json.Unmarshal(raw, &m) != nil || !keepModel(m, now) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, modelInfo(m))
	}
	return out, nil
}
