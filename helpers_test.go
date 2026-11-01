package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kit "github.com/TogetherWeOwn/cliproxy-provider-kit"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// now0 is a Wednesday afternoon (UTC): daily resets at 2026-11-05, weekly
// on Monday 2026-11-09, monthly on 2026-12-01.
var now0 = time.Date(2026, 11, 4, 15, 30, 0, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ptr[T any](v T) *T { return &v }

func rpcErr(t testing.TB, err error) *pluginabi.Error {
	t.Helper()
	var e *pluginabi.Error
	if !errors.As(err, &e) || e == nil {
		t.Fatalf("expected a public plugin error, got %T %v", err, err)
	}
	return e
}

const exampleKey = "example-key"

func storage(extra string) []byte {
	return []byte(`{"api_key":"` + exampleKey + `"` + extra + `}`)
}

// keyServer serves GET /key and GET /credits fixtures and counts calls.
type keyServer struct {
	*httptest.Server
	mu          sync.Mutex
	keyBody     []byte
	keyStatus   int
	credits     []byte
	creditsCode int
	keyCalls    atomic.Int32
	creditCalls atomic.Int32
	auth        []string
	gate        chan struct{}
}

func newKeyServer(t testing.TB, keyBody []byte) *keyServer {
	s := &keyServer{keyBody: keyBody, keyStatus: 200, creditsCode: 200}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auth = append(s.auth, r.URL.Path+" "+r.Header.Get("Authorization"))
		body, status, credits, creditsCode, gate := s.keyBody, s.keyStatus, s.credits, s.creditsCode, s.gate
		s.mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/json" {
			t.Errorf("unexpected request %s accept=%q", r.Method, r.Header.Get("Accept"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/key"):
			s.keyCalls.Add(1)
			if gate != nil {
				<-gate
			}
			w.WriteHeader(status)
			_, _ = w.Write(body)
		case strings.HasSuffix(r.URL.Path, "/credits"):
			s.creditCalls.Add(1)
			w.WriteHeader(creditsCode)
			_, _ = w.Write(credits)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *keyServer) set(body []byte, status int) {
	s.mu.Lock()
	s.keyBody, s.keyStatus = body, status
	s.mu.Unlock()
}

func (s *keyServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// harness wires the real components to a loopback key server and a clock.
type harness struct {
	clock *fakeClock
	srv   *keyServer
	keys  *keyState
	guard *guard
	quota *quotaProvider
	exec  executor
}

func newHarness(t testing.TB, keyBody []byte) *harness {
	h := &harness{clock: newClock(now0), srv: newKeyServer(t, keyBody)}
	client := kit.NewQuotaHTTPClient(nil)
	t.Cleanup(client.CloseIdleConnections)
	h.keys = newKeyState(client)
	h.keys.base, h.keys.now = h.srv.URL, h.clock.now
	h.guard = newGuard(h.keys)
	h.guard.now = h.clock.now
	h.guard.observeHost(pluginapi.HostConfigSummary{})
	h.quota = newQuotaProvider(h.keys, h.guard)
	h.keys.onInvalidate = h.quota.Invalidate
	h.exec = newExecutor(h.guard, h.keys)
	h.exec.now = h.clock.now
	return h
}

// fakeHost records upstream requests and answers them.
type fakeHost struct {
	mu       sync.Mutex
	requests []pluginapi.HTTPRequest
	status   int
	headers  http.Header
	body     []byte
	chunks   [][]byte
	gate     chan struct{}
}

func (f *fakeHost) record(req pluginapi.HTTPRequest) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
}
func (f *fakeHost) calls() []pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginapi.HTTPRequest(nil), f.requests...)
}
func (f *fakeHost) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.record(req)
	status := f.status
	if status == 0 {
		status = 200
	}
	return pluginapi.HTTPResponse{StatusCode: status, Headers: f.headers.Clone(), Body: f.body}, nil
}
func (f *fakeHost) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	f.record(req)
	status := f.status
	if status == 0 {
		status = 200
	}
	ch := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(ch)
		for i, c := range f.chunks {
			select {
			case ch <- pluginapi.HTTPStreamChunk{Payload: c}:
			case <-ctx.Done():
				return
			}
			if i == 0 && f.gate != nil {
				select {
				case <-f.gate:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{StatusCode: status, Headers: f.headers.Clone(), Chunks: ch}, nil
}
