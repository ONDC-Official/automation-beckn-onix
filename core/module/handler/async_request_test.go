package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
