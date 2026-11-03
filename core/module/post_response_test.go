package module

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/core/module/handler"
	"github.com/beckn-one/beckn-onix/pkg/response"
)

// ackThenForward behaves like the non-proxy route: it registers a hook that
// forwards the request, then writes the ACK. The hook stands in for a slow
// receiver and waits until the test releases it.
func ackThenForward(release <-chan struct{}, hookCtxErr chan<- error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		ctx := r.Context()
		handler.RegisterPostResponseHook(r, func() {
			<-release
			hookCtxErr <- ctx.Err()
		})
		response.SendAck(w)
	})
}

// TestPostResponseMiddleware_AckBeforeHooks checks that the caller gets its
// ACK right away, while the forwarding hook is still running.
func TestPostResponseMiddleware_AckBeforeHooks(t *testing.T) {
	release := make(chan struct{})
	hookCtxErr := make(chan error, 2)
	srv := httptest.NewServer(PostResponseMiddleware()(ackThenForward(release, hookCtxErr)))
	defer srv.Close()
	defer close(release)

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("caller did not get the ACK while the hook was running: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading ACK body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ACK"`) {
		t.Fatalf("unexpected response: %d %s", resp.StatusCode, body)
	}

	// A second call from the same client must not wait behind the first
	// call's hook on a reused connection.
	resp2, err := client.Post(srv.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("second call waited behind the first call's hook: %v", err)
	}
	_, _ = io.ReadAll(resp2.Body)
	resp2.Body.Close()
}

// TestPostResponseMiddleware_HookContextOutlivesCaller checks that once the
// ACK is sent, the caller closing its connection does not cancel the context
// the hook uses.
func TestPostResponseMiddleware_HookContextOutlivesCaller(t *testing.T) {
	release := make(chan struct{})
	hookCtxErr := make(chan error, 1)
	srv := httptest.NewServer(PostResponseMiddleware()(ackThenForward(release, hookCtxErr)))
	defer srv.Close()

	client := &http.Client{Timeout: 500 * time.Millisecond}
	if resp, err := client.Post(srv.URL, "application/json", strings.NewReader(`{}`)); err == nil {
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	// The caller is done and hangs up.
	client.CloseIdleConnections()
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case err := <-hookCtxErr:
		if err != nil {
			t.Fatalf("hook context was canceled after the caller left: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook never ran")
	}
}

// TestPostResponseMiddleware_StreamsWithoutHooks checks that a response with
// no hooks (for example proxy mode) is still passed straight through, so
// flushed data reaches the caller before the handler ends.
func TestPostResponseMiddleware_StreamsWithoutHooks(t *testing.T) {
	finish := make(chan struct{})
	srv := httptest.NewServer(PostResponseMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush not supported through the middleware: %v", err)
		}
		<-finish
		_, _ = w.Write([]byte("second\n"))
	})))
	defer srv.Close()
	defer close(finish)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, len("first\n"))
		_, _ = io.ReadFull(resp.Body, buf)
		got <- string(buf)
	}()
	select {
	case s := <-got:
		if s != "first\n" {
			t.Fatalf("unexpected first chunk %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("streamed data was held back")
	}
}

// TestPostResponseMiddleware_CallerCancelStillStopsHandler checks that while
// the handler is still working (before any response), the caller hanging up
// still cancels the request context. Proxy mode relies on this.
func TestPostResponseMiddleware_CallerCancelStillStopsHandler(t *testing.T) {
	canceled := make(chan struct{})
	srv := httptest.NewServer(PostResponseMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(5 * time.Second):
		}
	})))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(`{}`))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}

	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("caller hang-up did not cancel the request context")
	}
}

// TestPostResponseMiddleware_NoHooksUnchanged checks that a plain response
// with no hooks is passed through as is.
func TestPostResponseMiddleware_NoHooksUnchanged(t *testing.T) {
	h := PostResponseMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "1")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("body"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTeapot || rec.Body.String() != "body" || rec.Header().Get("X-Test") != "1" {
		t.Fatalf("response changed: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}
