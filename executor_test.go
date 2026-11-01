package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func execRequest(host *fakeHost, payload string) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{AuthID: "auth-1", Model: "x", Payload: []byte(payload), StorageJSON: storage(""), HTTPClient: host}
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
}

func TestBuildRequestStripsPrefixOnceAndKeepsFields(t *testing.T) {
	for in, want := range map[string]string{"openrouter/anthropic/claude-sonnet-4.5": "anthropic/claude-sonnet-4.5", "openrouter/openrouter/auto": "openrouter/auto", "openrouter/openrouter/free": "openrouter/free", "openrouter/~anthropic/claude-sonnet-latest": "~anthropic/claude-sonnet-latest", "openrouter/meta-llama/llama-3.3-70b-instruct:free": "meta-llama/llama-3.3-70b-instruct:free"} {
		payload := `{"model":"` + in + `","messages":[{"role":"user","content":"<hi> & bye"}],"provider":{"order":["a","b"],"allow_fallbacks":false},"models":["x/y"],"reasoning":{"effort":"high"},"plugins":[{"id":"web"}],"session_id":"s-1","temperature":0.2,"stream":true}`
		req, _, upstream, err := buildRequest([]byte(payload), storage(""), nil, ptr(false))
		if err != nil || upstream != want {
			t.Fatalf("%s: %q %v", in, upstream, err)
		}
		jsonEqual(t, req.Body, `{"model":"`+want+`","messages":[{"role":"user","content":"<hi> & bye"}],"provider":{"order":["a","b"],"allow_fallbacks":false},"models":["x/y"],"reasoning":{"effort":"high"},"plugins":[{"id":"web"}],"session_id":"s-1","temperature":0.2}`)
	}
}

func TestBuildRequestRejectsInvalidModels(t *testing.T) {
	for _, payload := range []string{`{"model":"anthropic/claude"}`, `{"model":"openrouter/"}`, `{"model":"openrouter/x"}`, `{"model":"openrouter/a b/c"}`, `{"model":7}`, `{}`} {
		_, _, _, err := buildRequest([]byte(payload), storage(""), nil, nil)
		if e := rpcErr(t, err); e.Code != "invalid_model" || e.HTTPStatus != 400 {
			t.Fatalf("%s: %+v", payload, e)
		}
	}
	for _, payload := range []string{`[]`, `"x"`, `null`, `{`} {
		_, _, _, err := buildRequest([]byte(payload), storage(""), nil, nil)
		if e := rpcErr(t, err); e.Code != "invalid_request" || e.HTTPStatus != 400 {
			t.Fatalf("%s: %+v", payload, e)
		}
	}
}

func TestExecuteRequestShape(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	host := &fakeHost{body: []byte(`{"id":"gen-1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`)}
	out, err := h.exec.Execute(context.Background(), execRequest(host, `{"model":"openrouter/anthropic/claude-sonnet-4.5","stream":true,"messages":[]}`))
	if err != nil || !bytes.Contains(out.Payload, []byte(`"content":"hi"`)) {
		t.Fatalf("%s %v", out.Payload, err)
	}
	calls := host.calls()
	if len(calls) != 1 {
		t.Fatalf("calls %d", len(calls))
	}
	req := calls[0]
	wantHeaders := http.Header{"Authorization": {"Bearer " + exampleKey}, "Content-Type": {"application/json"}, "Accept": {"application/json"}}
	if req.Method != http.MethodPost || req.URL != "https://openrouter.ai/api/v1/chat/completions" || !reflect.DeepEqual(req.Headers, wantHeaders) {
		t.Fatalf("%s %s %v", req.Method, req.URL, req.Headers)
	}
	jsonEqual(t, req.Body, `{"model":"anthropic/claude-sonnet-4.5","messages":[]}`)
}

func TestExecuteStreamRequestShapeAndUnbufferedRelay(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	sse := fixture(t, "ok_stream_processing_comments.sse")
	parts := bytes.SplitAfter(sse, []byte("\n\n"))
	host := &fakeHost{chunks: parts, gate: make(chan struct{}), headers: http.Header{"Content-Type": {"text/event-stream"}}}
	stream, err := h.exec.ExecuteStream(context.Background(), execRequest(host, `{"model":"openrouter/openrouter/free","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	select {
	case c := <-stream.Chunks:
		got.Write(c.Payload)
		close(host.gate)
	case <-time.After(5 * time.Second):
		t.Fatal("stream was buffered")
	}
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		got.Write(c.Payload)
	}
	if !bytes.Equal(got.Bytes(), sse) {
		t.Fatalf("stream altered:\n%s", got.Bytes())
	}
	req := host.calls()[0]
	if req.Headers.Get("Accept") != "text/event-stream" {
		t.Fatalf("accept %q", req.Headers.Get("Accept"))
	}
	jsonEqual(t, req.Body, `{"model":"openrouter/free","messages":[],"stream":true}`)
}

func TestMidStreamErrorIsRelayedRaw(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	sse := fixture(t, "ok_200_stream_with_error_chunk.sse")
	host := &fakeHost{chunks: [][]byte{sse}}
	stream, err := h.exec.ExecuteStream(context.Background(), execRequest(host, `{"model":"openrouter/a/b"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	for c := range stream.Chunks {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		got.Write(c.Payload)
	}
	if !bytes.Equal(got.Bytes(), sse) {
		t.Fatal("mid-stream error chunk was altered")
	}
}

func TestStreamFailureIsClassified(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	host := &fakeHost{status: 403, chunks: [][]byte{fixture(t, "err_403_key_limit.json")}}
	_, err := h.exec.ExecuteStream(context.Background(), execRequest(host, `{"model":"openrouter/a/b"}`))
	if e := rpcErr(t, err); e.Code != "quota_exhausted" || e.HTTPStatus != 429 || e.RetryAfterSeconds != secondsUntil("2026-11-05T00:00:00Z") {
		t.Fatalf("%+v", e)
	}
}

func TestHttpRequestIgnoresCallerDestinationAndHeaders(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	host := &fakeHost{body: []byte(`{"choices":[{"index":0}]}`)}
	_, err := h.exec.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{AuthID: "auth-1", Method: http.MethodPut, URL: "https://attacker.example/steal", Headers: http.Header{"X-Extra": {"1"}, "Authorization": {"Bearer other"}}, Body: []byte(`{"model":"openrouter/a/b","stream":false,"x":1}`), StorageJSON: storage(""), HTTPClient: host})
	if err != nil {
		t.Fatal(err)
	}
	req := host.calls()[0]
	if req.Method != http.MethodPost || req.URL != "https://openrouter.ai/api/v1/chat/completions" || req.Headers.Get("X-Extra") != "" || req.Headers.Get("Authorization") != "Bearer "+exampleKey || len(req.Headers) != 3 {
		t.Fatalf("%s %s %v", req.Method, req.URL, req.Headers)
	}
	jsonEqual(t, req.Body, `{"model":"a/b","stream":false,"x":1}`)
}

func TestGuardRunsBeforeDispatch(t *testing.T) {
	h := newHarness(t, fixture(t, "key_no_limit.json"))
	host := &fakeHost{}
	_, err := h.exec.Execute(context.Background(), execRequest(host, `{"model":"openrouter/a/b"}`))
	if e := rpcErr(t, err); e.Code != "key_limit_required" || e.HTTPStatus != 403 {
		t.Fatalf("%+v", e)
	}
	_, err = h.exec.ExecuteStream(context.Background(), execRequest(host, `{"model":"openrouter/a/b"}`))
	if e := rpcErr(t, err); e.Code != "key_limit_required" {
		t.Fatalf("%+v", e)
	}
	_, err = h.exec.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{AuthID: "auth-1", Body: []byte(`{"model":"openrouter/a/b"}`), StorageJSON: storage(""), HTTPClient: host})
	if e := rpcErr(t, err); e.Code != "key_limit_required" {
		t.Fatalf("%+v", e)
	}
	if n := len(host.calls()); n != 0 {
		t.Fatalf("guard refusal dispatched %d upstream calls", n)
	}
}

func TestErrorOnlySuccessBodyIsClassified(t *testing.T) {
	h := newHarness(t, fixture(t, "key_monthly_limit.json"))
	host := &fakeHost{body: fixture(t, "ok_200_error_only.json"), headers: http.Header{"Retry-After": {"4"}}}
	_, err := h.exec.Execute(context.Background(), execRequest(host, `{"model":"openrouter/a/b"}`))
	if e := rpcErr(t, err); e.HTTPStatus != 429 || e.Code != "rate_limited" || e.RetryAfterSeconds != 4 {
		t.Fatalf("%+v", e)
	}
	for body, want := range map[string]int{`{"error":{"message":"x"}}`: 502, `{"error":{"code":200}}`: 502, `{"error":{"code":503}}`: 503} {
		if got, ok := errorOnlyStatus([]byte(body)); !ok || got != want {
			t.Fatalf("%s: %d %v", body, got, ok)
		}
	}
	for _, body := range []string{`{"choices":[{"index":0}],"error":{"code":500}}`, `{"choices":[]}`, `{"error":null}`, `not json`} {
		if _, ok := errorOnlyStatus([]byte(body)); ok {
			t.Fatalf("%s classified as error-only", body)
		}
	}
}
