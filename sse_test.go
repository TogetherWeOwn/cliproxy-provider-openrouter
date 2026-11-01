package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func feed(chunks ...pluginapi.ExecutorStreamChunk) <-chan pluginapi.ExecutorStreamChunk {
	in := make(chan pluginapi.ExecutorStreamChunk, len(chunks))
	for _, c := range chunks {
		in <- c
	}
	close(in)
	return in
}

func bytesChunk(s string) pluginapi.ExecutorStreamChunk {
	return pluginapi.ExecutorStreamChunk{Payload: []byte(s)}
}

func collect(t *testing.T, out <-chan pluginapi.ExecutorStreamChunk) (payloads []string, errs []error) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-out:
			if !ok {
				return payloads, errs
			}
			if c.Err != nil {
				errs = append(errs, c.Err)
				continue
			}
			payloads = append(payloads, string(c.Payload))
		case <-timeout:
			t.Fatal("stream did not close")
		}
	}
}

// Several events and a comment in one write, a frame split across writes,
// CRLF line ends, a multi-line data field, [DONE], and a final frame with no
// blank line.
var packedStream = []string{
	": OPENROUTER PROCESSING\n\ndata: {\"a\":1}\n\ndata: {\"b\"",
	":2}\r\n\r\n: OPENROUTER PROCESSING\r\n\r\ndata: {\"c\":\n",
	"data: 3}\n\nid: 7\nevent: x\ndata:{\"d\":4}\n\ndata: [DONE]\n\ndata: {\"e\":5}",
}

func TestFrameEventsBareForOpenAIChat(t *testing.T) {
	var chunks []pluginapi.ExecutorStreamChunk
	for _, s := range packedStream {
		chunks = append(chunks, bytesChunk(s))
	}
	got, errs := collect(t, frameEvents(context.Background(), feed(chunks...), true))
	want := []string{`{"a":1}`, `{"b":2}`, "{\"c\":\n3}", `{"d":4}`, `{"e":5}`}
	if len(errs) != 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

func TestFrameEventsDataFramedForTranslatedRoutes(t *testing.T) {
	var chunks []pluginapi.ExecutorStreamChunk
	for _, s := range packedStream {
		chunks = append(chunks, bytesChunk(s))
	}
	got, _ := collect(t, frameEvents(context.Background(), feed(chunks...), false))
	want := []string{`data: {"a":1}`, `data: {"b":2}`, "data: {\"c\":\n3}", `data: {"d":4}`, `data: {"e":5}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestFrameEventsPassesErrorsThrough(t *testing.T) {
	boom := errors.New("boom")
	got, errs := collect(t, frameEvents(context.Background(), feed(bytesChunk("data: {\"a\":1}\n\ndata: {\"trunc"), pluginapi.ExecutorStreamChunk{Payload: []byte("ated\":"), Err: boom}), false))
	// Complete events before the error are delivered; a truncated frame is not.
	if !reflect.DeepEqual(got, []string{`data: {"a":1}`}) || len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("got %q errs %v", got, errs)
	}
}

func TestFrameEventsStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan pluginapi.ExecutorStreamChunk)
	out := frameEvents(ctx, in, true)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("chunk after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not closed after cancel")
	}
}

func TestClientWantsBareJSON(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/chat/completions":   true,
		"/v1/completions":        true,
		"/v1/messages":           false,
		"/v1/responses":          false,
		"/v1beta/models/*action": false,
		"":                       false,
	} {
		md := map[string]any{}
		if path != "" {
			md["request_path"] = path
		}
		if got := clientWantsBareJSON(pluginapi.ExecutorRequest{Format: "openai", SourceFormat: "openai", Metadata: md}); got != want {
			t.Fatalf("%q: %v", path, got)
		}
	}
}

func TestFrameEventsBoundsAnEvent(t *testing.T) {
	big := make([]byte, maxEventBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	got, errs := collect(t, frameEvents(context.Background(), feed(bytesChunk("data: {\"a\":1}\n\ndata: "), pluginapi.ExecutorStreamChunk{Payload: big}), true))
	if !reflect.DeepEqual(got, []string{`{"a":1}`}) || len(errs) != 1 || rpcErr(t, errs[0]).Code != "invalid_stream" {
		t.Fatalf("got %d events, errs %v", len(got), errs)
	}
}
