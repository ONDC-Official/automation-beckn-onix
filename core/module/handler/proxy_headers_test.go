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

// TestSanitizedProxyHeader verifies that only the ONDC signature and content
// negotiation headers survive, and that caller-supplied headers which can trip
// heterogeneous NP upstreams (Accept-Encoding, Cookie, User-Agent, forwarding
// and internal headers) are dropped.
func TestSanitizedProxyHeader(t *testing.T) {
	in := http.Header{}
	in.Set(model.AuthHeaderSubscriber, `Signature keyId="sub|key|ed25519"`)
	in.Set(model.AuthHeaderGateway, `Signature keyId="gw|key|ed25519"`)
	in.Set("Content-Type", "application/json; charset=utf-8")
	in.Set("Accept-Encoding", "gzip, br")
	in.Set("Cookie", "session_id=abc; custom-response-body=xyz")
	in.Set("User-Agent", "axios/1.13.4")
	in.Set("X-Forwarded-Host", "workbench.ondc.tech")
	in.Set("X-Module-Name", "workbench")
	in.Set("Cf-Connecting-Ip", "1.2.3.4")

	out := sanitizedProxyHeader(in)

	// Preserved.
	if got := out.Get(model.AuthHeaderSubscriber); got != `Signature keyId="sub|key|ed25519"` {
		t.Errorf("subscriber auth header not preserved: %q", got)
	}
	if got := out.Get(model.AuthHeaderGateway); got != `Signature keyId="gw|key|ed25519"` {
		t.Errorf("gateway auth header not preserved: %q", got)
	}
	if got := out.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("content-type not preserved: %q", got)
	}
	if got := out.Get("Accept"); got != "application/json" {
		t.Errorf("Accept not set: %q", got)
	}

	// Dropped.
	for _, h := range []string{"Accept-Encoding", "Cookie", "User-Agent", "X-Forwarded-Host", "X-Module-Name", "Cf-Connecting-Ip"} {
		if v := out.Get(h); v != "" {
			t.Errorf("caller header %q should have been dropped, got %q", h, v)
		}
	}
}

// TestSanitizedProxyHeader_DefaultsContentType ensures a request without a
// Content-Type still goes out as application/json.
func TestSanitizedProxyHeader_DefaultsContentType(t *testing.T) {
	out := sanitizedProxyHeader(http.Header{})
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Errorf("expected default content-type application/json, got %q", got)
	}
}

// TestProxy_ForwardsCleanHeadersAndBody drives the real proxy() against a
// recording upstream: the NP must receive the signed body verbatim and only the
// sanitized header set — no Accept-Encoding or Cookie leaking through.
func TestProxy_ForwardsCleanHeadersAndBody(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"message":{"ack":{"status":"ACK"}}}`))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	body := []byte(`{"context":{"action":"search"},"message":{}}`)

	// Inbound request as it reaches the proxy: the signed body rides on r.Body
	// (ctx.Body is only used for logging), plus a pile of caller headers.
	r := httptest.NewRequest(http.MethodPost, "https://workbench.ondc.tech/api-service/x/mock/search", bytes.NewReader(body))
	r.Header.Set(model.AuthHeaderSubscriber, `Signature keyId="sub|key|ed25519"`)
	r.Header.Set("Accept-Encoding", "gzip, br")
	r.Header.Set("Cookie", "session_id=abc")
	r.Header.Set("User-Agent", "axios/1.13.4")

	ctx := &model.StepContext{
		Context: context.Background(),
		Request: r,
		Body:    body,
		Route:   &model.Route{TargetType: "url", URL: target, ActAsProxy: true},
	}

	rec := httptest.NewRecorder()
	proxy(ctx, r, rec, http.DefaultClient)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if string(gotBody) != string(body) {
		t.Errorf("signed body altered in transit: got %q want %q", gotBody, body)
	}
	if gotHeader.Get(model.AuthHeaderSubscriber) == "" {
		t.Error("signature header did not reach upstream")
	}
	if v := gotHeader.Get("Cookie"); v != "" {
		t.Errorf("caller Cookie leaked to NP: %q", v)
	}
	// Accept-Encoding must not be the caller's value; the transport may add its own
	// gzip for transparent decompression, but "gzip, br" must not pass through.
	if v := gotHeader.Get("Accept-Encoding"); v == "gzip, br" {
		t.Errorf("caller Accept-Encoding leaked to NP: %q", v)
	}
	if v := gotHeader.Get("User-Agent"); v == "axios/1.13.4" {
		t.Errorf("caller User-Agent leaked to NP: %q", v)
	}
}
