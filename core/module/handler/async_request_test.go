package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/model"
)

// newAsyncStepCtx builds the StepContext that route() hands to makeAsyncRequest:
// the inbound request carries the headers the sign step has set on it.
func newAsyncStepCtx(t *testing.T, ctx context.Context, targetURL string, body []byte) *model.StepContext {
	t.Helper()
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatalf("bad target url: %v", err)
	}
	r := httptest.NewRequest(http.MethodPost, "http://adapter.local/bap/caller/search", bytes.NewReader(body))
	return &model.StepContext{
		Context: ctx,
		Request: r,
		Body:    body,
		Route:   &model.Route{TargetType: "url", URL: target},
	}
}

// TestMakeAsyncRequest_ForwardsSignatureHeaders checks that the signature the
// sign step put on the request reaches the receiver. Without it the receiver
// gets an unsigned call and rejects it.
func TestMakeAsyncRequest_ForwardsSignatureHeaders(t *testing.T) {
	var got http.Header
	var gotBody []byte
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	body := []byte(`{"context":{"action":"search"},"message":{}}`)
	stepCtx := newAsyncStepCtx(t, context.Background(), receiver.URL, body)
	stepCtx.Request.Header.Set(model.AuthHeaderSubscriber, `Signature keyId="bap.example.com|key1|ed25519"`)
	stepCtx.Request.Header.Set(model.AuthHeaderGateway, `Signature keyId="gateway.example.com|key2|ed25519"`)

	if err := makeAsyncRequest(stepCtx, stepCtx, receiver.Client()); err != nil {
		t.Fatalf("makeAsyncRequest returned error: %v", err)
	}

	if v := got.Get(model.AuthHeaderSubscriber); v != `Signature keyId="bap.example.com|key1|ed25519"` {
		t.Errorf("%s header not forwarded, got %q", model.AuthHeaderSubscriber, v)
	}
	if v := got.Get(model.AuthHeaderGateway); v != `Signature keyId="gateway.example.com|key2|ed25519"` {
		t.Errorf("%s header not forwarded, got %q", model.AuthHeaderGateway, v)
	}
	if string(gotBody) != string(body) {
		t.Errorf("body changed in transit: got %q want %q", gotBody, body)
	}
}

// TestMakeAsyncRequest_NoSignatureHeaders checks that no empty signature
// headers are invented when the request was not signed.
func TestMakeAsyncRequest_NoSignatureHeaders(t *testing.T) {
	var got http.Header
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	stepCtx := newAsyncStepCtx(t, context.Background(), receiver.URL, []byte(`{}`))
	if err := makeAsyncRequest(stepCtx, stepCtx, receiver.Client()); err != nil {
		t.Fatalf("makeAsyncRequest returned error: %v", err)
	}
	for _, h := range []string{model.AuthHeaderSubscriber, model.AuthHeaderGateway} {
		if _, ok := got[http.CanonicalHeaderKey(h)]; ok {
			t.Errorf("unexpected %s header on unsigned request: %q", h, got.Get(h))
		}
	}
}

type asyncTestCtxKey struct{}

// ctxValueTransport records a value from the outbound request's context, so the
// test can check that values (trace ids, logger fields) still flow through.
type ctxValueTransport struct {
	base http.RoundTripper
	seen any
}

func (c *ctxValueTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.seen = r.Context().Value(asyncTestCtxKey{})
	return c.base.RoundTrip(r)
}

// TestMakeAsyncRequest_SurvivesInboundCancel checks that the outbound call
// finishes even when the inbound request's context is canceled while the
// receiver is still working. The inbound context is canceled as soon as the
// caller's connection closes, which happens long before a slow receiver answers.
func TestMakeAsyncRequest_SurvivesInboundCancel(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release // a slow receiver, still working after the caller is gone
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"ack":{"status":"ACK"}}}`))
	}))
	defer receiver.Close()

	inbound, cancel := context.WithCancel(context.WithValue(context.Background(), asyncTestCtxKey{}, "trace-123"))
	defer cancel()
	stepCtx := newAsyncStepCtx(t, inbound, receiver.URL, []byte(`{}`))

	transport := &ctxValueTransport{base: receiver.Client().Transport}
	client := &http.Client{Transport: transport}

	done := make(chan error, 1)
	go func() { done <- makeAsyncRequest(stepCtx, stepCtx, client) }()

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the receiver")
	}
	cancel() // the caller hangs up
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("outbound call was cut off by the inbound cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("makeAsyncRequest did not return")
	}
	if transport.seen != "trace-123" {
		t.Errorf("context values not kept on the outbound request, got %v", transport.seen)
	}
}
