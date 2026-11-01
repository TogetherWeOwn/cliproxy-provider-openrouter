package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// maxEventBytes bounds one buffered SSE event (OpenRouter events are small;
// a stream without line breaks must not grow memory without limit).
const maxEventBytes = 8 << 20

// The host consumes one SSE event per stream chunk, but OpenRouter's raw bytes
// can carry several events, comments and [DONE] in one read. frameEvents
// re-frames the upstream stream into exactly one chunk per data event:
//
//   - When the client speaks OpenAI chat, the host skips translation for this
//     same-format plugin stream and the OpenAI handler writes each chunk as
//     "data: %s\n\n" (v8.0.49 internal/pluginhost/adapters_executors.go:753,
//     sdk/api/handlers/openai/openai_handlers.go:581), so the chunk is the
//     bare JSON payload.
//   - Otherwise a host translator reads the chunk and needs one "data:"-framed
//     event per call (internal/translator/openai/claude/
//     openai_claude_response.go:140), so the chunk is "data: <json>".
//
// Comment lines (": OPENROUTER PROCESSING") and [DONE] are dropped: the host
// and the handlers append their own terminator (adapters_executors.go:828-848).
// Errors pass through; a frame cut short by an error is not delivered.
func frameEvents(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, bare bool) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		var line []byte   // the current, incomplete line
		var data []string // data lines of the current event
		size := 0         // bytes held in data
		send := func(c pluginapi.ExecutorStreamChunk) bool {
			select {
			case out <- c:
				return true
			case <-ctx.Done():
				return false
			}
		}
		dispatch := func() bool {
			if len(data) == 0 {
				return true
			}
			payload := strings.Join(data, "\n")
			data, size = data[:0], 0
			if strings.TrimSpace(payload) == "[DONE]" {
				return true
			}
			if !bare {
				payload = "data: " + payload
			}
			return send(pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
		}
		// field handles one complete line (SSE: CRLF, LF or CR terminated).
		field := func(l []byte) bool {
			l = bytes.TrimSuffix(l, []byte("\r"))
			switch {
			case len(l) == 0:
				return dispatch()
			case l[0] == ':':
				// comment
			case bytes.HasPrefix(l, []byte("data:")):
				v := l[len("data:"):]
				if len(v) > 0 && v[0] == ' ' {
					v = v[1:]
				}
				data = append(data, string(v))
				size += len(v)
			}
			// Other fields (event, id, retry) carry nothing the host reads.
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case c, ok := <-in:
				if !ok {
					if len(line) > 0 && !field(line) {
						return
					}
					dispatch()
					return
				}
				buf := append(line, c.Payload...)
				for {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						break
					}
					if !field(buf[:i]) {
						return
					}
					buf = buf[i+1:]
				}
				line = append(line[:0:0], buf...)
				if len(line)+size > maxEventBytes {
					send(pluginapi.ExecutorStreamChunk{Err: &pluginabi.Error{Code: "invalid_stream", Message: "invalid_stream: upstream event exceeds 8 MiB", HTTPStatus: http.StatusBadGateway}})
					return
				}
				if c.Err != nil {
					send(pluginapi.ExecutorStreamChunk{Err: c.Err})
					return
				}
			}
		}
	}()
	return out
}

// clientWantsBareJSON reports a client on an OpenAI chat or completions
// route, the only case in which the host passes this plugin's stream through
// untranslated. The plugin cannot read the client format from the request:
// the host sets SourceFormat and Format to the plugin's own declared format
// ("openai") whatever the client speaks (prepareExecutorCall). The inbound
// route the host records in the request metadata is the reliable signal.
// Without it the chunk stays "data:"-framed, the host's documented chunk form.
func clientWantsBareJSON(r pluginapi.ExecutorRequest) bool {
	path, _ := r.Metadata["request_path"].(string)
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/v1/completions")
}
