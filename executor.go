package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executor dispatches OpenAI chat completions to OpenRouter through the host
// HTTP client. Every path (Execute, ExecuteStream, HttpRequest) builds its
// upstream request in buildRequest and passes the spend guard first.
type executor struct {
	guard *guard
	keys  *keyState
	now   func() time.Time
}

func newExecutor(g *guard, keys *keyState) executor {
	return executor{guard: g, keys: keys, now: time.Now}
}

func (executor) Identifier() string { return provider }

// passthrough binds the guard and the classifier to one auth and model.
// stream nil keeps the stream flag of the body (raw HTTP requests).
func (e executor) passthrough(authID, model string, stream *bool) kit.Passthrough {
	build := func(ctx context.Context, payload, storage []byte, metadata map[string]any) (pluginapi.HTTPRequest, error) {
		req, c, upstream, err := buildRequest(payload, storage, metadata, stream)
		if err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		if err = e.guard.admit(ctx, authID, c, storage, metadata, upstream); err != nil {
			return pluginapi.HTTPRequest{}, err
		}
		return req, nil
	}
	return kit.Passthrough{
		Provider: provider,
		BuildRequest: func(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.HTTPRequest, error) {
			return build(ctx, r.Payload, r.StorageJSON, r.AuthMetadata)
		},
		// Raw HTTP execution cannot inject destinations, credentials or headers.
		PrepareHTTP: func(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.HTTPRequest, error) {
			return build(ctx, r.Body, r.StorageJSON, r.Metadata)
		},
		ClassifyError: func(resp pluginapi.HTTPResponse) error { return e.classify(authID, model, resp) },
	}
}

// classify maps an upstream failure and applies its effect: an exhaustion
// latches the auth (or its :free models) until the retry time, before the
// reading is invalidated, so no admission slips between the two.
func (e executor) classify(authID, model string, resp pluginapi.HTTPResponse) error {
	now := e.now()
	err, eff := classifyFailure(resp, now, e.keys.peek(authID), model)
	if eff == effectNone || authID == "" {
		return err
	}
	until := now.Add(time.Duration(err.RetryAfterSeconds) * time.Second)
	switch eff {
	case effectSpend:
		e.keys.latch(authID, latchSpend, until)
	case effectFree:
		e.keys.latch(authID, latchFree, until)
	}
	e.keys.invalidate(authID)
	return err
}

func (e executor) Execute(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	r.Stream = false
	model := upstreamModel(r.Payload)
	out, err := e.passthrough(r.AuthID, model, &r.Stream).Execute(ctx, r)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	// OpenRouter can answer 200 with only an error object.
	if status, ok := errorOnlyStatus(out.Payload); ok {
		return pluginapi.ExecutorResponse{}, e.classify(r.AuthID, model, pluginapi.HTTPResponse{StatusCode: status, Headers: out.Headers, Body: out.Payload})
	}
	return out, nil
}

func (e executor) ExecuteStream(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	r.Stream = true
	return e.passthrough(r.AuthID, upstreamModel(r.Payload), &r.Stream).ExecuteStream(ctx, r)
}

func (e executor) HttpRequest(ctx context.Context, r pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return e.passthrough(r.AuthID, upstreamModel(r.Body), nil).HttpRequest(ctx, r)
}

func (e executor) CountTokens(ctx context.Context, r pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return kit.Passthrough{Provider: provider}.CountTokens(ctx, r)
}

// errorOnlyStatus reports a JSON object with a top-level error and no
// non-empty choices, and the status its error.code names (default 502).
func errorOnlyStatus(body []byte) (int, bool) {
	o, ok := decodeObject(body)
	if !ok {
		return 0, false
	}
	if raw, ok := o["error"]; !ok || isNull(raw) {
		return 0, false
	}
	var choices []json.RawMessage
	if raw, ok := o["choices"]; ok && json.Unmarshal(raw, &choices) == nil && len(choices) > 0 {
		return 0, false
	}
	u, _ := parseUpstreamError(body)
	if u.hasCode && u.code >= 400 && u.code <= 599 {
		return u.code, true
	}
	return http.StatusBadGateway, true
}

func invalidModel() *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_model", Message: "invalid_model: expected an openrouter/ model", HTTPStatus: http.StatusBadRequest}
}

// upstreamModel returns the requested model with the prefix stripped once,
// for classification only; buildRequest validates it.
func upstreamModel(payload []byte) string {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(payload, &body)
	return strings.TrimPrefix(body.Model, modelPrefix)
}

func validUpstreamModel(id string) bool {
	if !strings.Contains(id, "/") || len(id) > 256 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] <= ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

// buildRequest rewrites only the model (prefix stripped once) and the stream
// flag; every other field passes through. stream nil keeps the body's flag.
func buildRequest(payload, storage []byte, metadata map[string]any, stream *bool) (pluginapi.HTTPRequest, credentials, string, error) {
	c, err := credentialsFrom(storage, metadata)
	if err != nil {
		return pluginapi.HTTPRequest{}, c, "", err
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil || body == nil {
		return pluginapi.HTTPRequest{}, c, "", &pluginabi.Error{Code: "invalid_request", Message: "invalid_request: payload must be a JSON object", HTTPStatus: http.StatusBadRequest}
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil || !strings.HasPrefix(model, modelPrefix) {
		return pluginapi.HTTPRequest{}, c, "", invalidModel()
	}
	model = strings.TrimPrefix(model, modelPrefix)
	if !validUpstreamModel(model) {
		return pluginapi.HTTPRequest{}, c, "", invalidModel()
	}
	body["model"], _ = json.Marshal(model)
	streaming := false
	if stream != nil {
		streaming = *stream
		if streaming {
			body["stream"] = json.RawMessage("true")
		} else {
			delete(body, "stream")
		}
	} else if raw, ok := body["stream"]; ok {
		_ = json.Unmarshal(raw, &streaming)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return pluginapi.HTTPRequest{}, c, "", &pluginabi.Error{Code: "invalid_request", Message: "invalid_request: payload cannot be encoded", HTTPStatus: http.StatusBadRequest}
	}
	accept := "application/json"
	if streaming {
		accept = "text/event-stream"
	}
	headers := http.Header{"Authorization": {"Bearer " + c.apiKey}, "Content-Type": {"application/json"}, "Accept": {accept}}
	return pluginapi.HTTPRequest{Method: http.MethodPost, URL: selectedBase(upstreamBase) + "/chat/completions", Headers: headers, Body: raw}, c, model, nil
}

var _ pluginapi.ProviderExecutor = executor{}
