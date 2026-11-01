package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// Errors pass through; a frame cut short by an error is not delivered. An
// event over 8 MiB, or a multi-line event that is not one JSON value, ends the
// stream with an invalid_stream error.
func frameEvents(ctx context.Context, in <-chan pluginapi.ExecutorStreamChunk, bare bool) <-chan pluginapi.ExecutorStreamChunk {
	out := make(chan pluginapi.ExecutorStreamChunk)
	go func() {
		defer close(out)
		var line []byte   // the current, incomplete line
		var data []string // data lines of the current event
		size := 0         // bytes held in data
		send := func(c pluginapi.ExecutorStreamChunk) error {
			select {
			case out <- c:
				return nil
			case <-ctx.Done():
				return errConsumerGone
			}
		}
		dispatch := func() error {
			if len(data) == 0 {
				return nil
			}
			payload := strings.Join(data, "\n")
			data, size = data[:0], 0
			if strings.TrimSpace(payload) == "[DONE]" {
				return nil
			}
			// One chunk is one line downstream: an event split over several
			// data lines must be a single JSON value, sent compacted.
			if strings.Contains(payload, "\n") {
				var b bytes.Buffer
				if json.Compact(&b, []byte(payload)) != nil {
					return invalidStream("invalid_stream: multi-line upstream event is not JSON")
				}
				payload = b.String()
			}
			if !bare {
				payload = "data: " + payload
			}
			return send(pluginapi.ExecutorStreamChunk{Payload: []byte(payload)})
		}
		// field handles one complete line (SSE: CRLF or LF terminated).
		field := func(l []byte) error {
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
				if size+len(v) > maxEventBytes {
					return errEventTooLarge
				}
				data = append(data, string(v))
				size += len(v)
			}
			// Other fields (event, id, retry) carry nothing the host reads.
			return nil
		}
		// run consumes the input; it returns nil at the end of the stream.
		run := func() error {
			for {
				select {
				case <-ctx.Done():
					return errConsumerGone
				case c, ok := <-in:
					if !ok {
						if len(line) > 0 {
							if err := field(line); err != nil {
								return err
							}
						}
						return dispatch()
					}
					buf := append(line, c.Payload...)
					for {
						i := bytes.IndexByte(buf, '\n')
						if i < 0 {
							break
						}
						if err := field(buf[:i]); err != nil {
							return err
						}
						buf = buf[i+1:]
					}
					line = append(line[:0:0], buf...)
					if len(line)+size > maxEventBytes {
						return errEventTooLarge
					}
					if c.Err != nil {
						return c.Err
					}
				}
			}
		}
		if err := run(); err != nil && err != errConsumerGone {
			_ = send(pluginapi.ExecutorStreamChunk{Err: err})
		}
	}()
	return out
}

var (
	errConsumerGone  = errors.New("stream consumer gone")
	errEventTooLarge = invalidStream("invalid_stream: upstream event exceeds 8 MiB")
)

func invalidStream(msg string) *pluginabi.Error {
	return &pluginabi.Error{Code: "invalid_stream", Message: msg, HTTPStatus: http.StatusBadGateway}
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
