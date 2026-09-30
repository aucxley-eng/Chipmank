package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aucxley/chipmank/services/ingress/internal/verify"
)

const (
	appID       = "app-123"
	phoneNumber = "pn-456"
	wabaID      = "waba-789"
)

var appSecret = []byte("integration-test-secret")

// body built as raw bytes on purpose: the handler must hash and store these
// exact bytes, so the test controls them literally.
var inboundBody = []byte(`{"object":"whatsapp_business_account","entry":[{"id":"` + wabaID + `","changes":[{"field":"messages","value":{"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550000000","phone_number_id":"` + phoneNumber + `"},"messages":[{"from":"15551230000","id":"wamid.HBgLMTY1","timestamp":"1717171717","type":"text","text":{"body":"hello"}}]}}]}]}`)

// --------------------------------------------------------------- doubles

type stubResolver struct {
	secret []byte
	err    error
	calls  int
}

func (s *stubResolver) AppCredential(context.Context, string) (Credential, error) {
	s.calls++
	if s.err != nil {
		return Credential{}, s.err
	}
	return Credential{Secret: s.secret, Ref: "env:TEST_APP_SECRET"}, nil
}

type recordingAppender struct {
	outcome  Outcome
	err      error
	calls    int
	gotKeys  []string
	gotBytes [][]byte
}

func (r *recordingAppender) Append(_ context.Context, d Delivery) (Outcome, error) {
	r.calls++
	r.gotKeys = append(r.gotKeys, d.DeliveryKey)
	// Copy, because the handler may reuse its buffer.
	cp := make([]byte, len(d.RawPayload))
	copy(cp, d.RawPayload)
	r.gotBytes = append(r.gotBytes, cp)
	return r.outcome, r.err
}

// ----------------------------------------------------------------- setup

type fixture struct {
	handler  *Handler
	resolver *stubResolver
	appender *recordingAppender
}

func newFixture(resolveErr, appendErr error) *fixture {
	f := &fixture{
		resolver: &stubResolver{secret: appSecret, err: resolveErr},
		appender: &recordingAppender{outcome: OutcomeStored, err: appendErr},
	}
	f.handler = New(Options{
		Resolve: f.resolver,
		Append:  f.appender,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return time.Unix(1750000000, 0).UTC() },
	})
	return f
}

const webhookPath = "/webhooks/meta/" + appID

// post sends body signed with secret. A nil secret omits the signature header.
func post(t *testing.T, h http.Handler, webhookPath string, body []byte, secret []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != nil {
		req.Header.Set(verify.SignatureHeader, verify.Sign(secret, body))
	}
	return serve(t, h, req)
}

func serve(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ----------------------------------------------------------------- tests

func TestValidWebhookIsStoredAndAcknowledged(t *testing.T) {
	f := newFixture(nil, nil)
	rec := post(t, f.handler.Routes(), webhookPath, inboundBody, appSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}
	if f.appender.calls != 1 {
		t.Fatalf("want exactly 1 append, got %d", f.appender.calls)
	}

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got["status"] != string(OutcomeStored) {
		t.Fatalf("want outcome %q, got %q", OutcomeStored, got["status"])
	}
	if got["delivery_key"] == "" {
		t.Fatal("want a delivery key in the response")
	}
}

// The bytes handed to the appender must be the bytes that were signed. If the
// handler normalised, re-indented or re-encoded the payload, Meta's digest
// would no longer match what we store.
func TestAppenderReceivesExactSignedBytes(t *testing.T) {
	f := newFixture(nil, nil)
	rec := post(t, f.handler.Routes(), webhookPath, inboundBody, appSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if len(f.appender.gotBytes) != 1 {
		t.Fatalf("want 1 stored body, got %d", len(f.appender.gotBytes))
	}
	if !bytes.Equal(f.appender.gotBytes[0], inboundBody) {
		t.Fatalf("stored bytes differ from signed bytes\n got: %s\nwant: %s",
			f.appender.gotBytes[0], inboundBody)
	}
}

// The invariant that protects against silent message loss: a failed durable
// write must never be acknowledged, because Meta stops retrying on 2xx.
func TestAppendFailureReturns503Not200(t *testing.T) {
	f := newFixture(nil, errors.New("connection reset"))
	rec := post(t, f.handler.Routes(), webhookPath, inboundBody, appSecret)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 so Meta retries, got %d", rec.Code)
	}
	if rec.Code >= 200 && rec.Code < 300 {
		t.Fatal("2xx on a failed append would silently drop the webhook")
	}
	if f.appender.calls != 1 {
		t.Fatalf("want 1 append attempt, got %d", f.appender.calls)
	}
}

func TestSecretLookupFailureReturns503(t *testing.T) {
	f := newFixture(errors.New("vault timeout"), nil)
	rec := post(t, f.handler.Routes(), webhookPath, inboundBody, appSecret)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if f.appender.calls != 0 {
		t.Fatal("must not append without a verified secret")
	}
}

func TestUnknownAppReturns404AndStoresNothing(t *testing.T) {
	f := newFixture(ErrUnknownApp, nil)
	rec := post(t, f.handler.Routes(), webhookPath, inboundBody, appSecret)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if f.appender.calls != 0 {
		t.Fatal("unknown app must not be stored")
	}
}

func TestInvalidSignatureIs401AndStoresNothing(t *testing.T) {
	cases := map[string][]byte{
		"wrong secret": []byte("not-the-secret"),
		"empty secret": {},
		"nil secret":   nil,
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(nil, nil)
			rec := post(t, f.handler.Routes(), webhookPath, inboundBody, secret)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", rec.Code)
			}
			if f.appender.calls != 0 {
				t.Fatal("unauthenticated body must never reach the database")
			}
		})
	}
}

func TestMissingSignatureHeaderIs401(t *testing.T) {
	f := newFixture(nil, nil)
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(inboundBody))
	req.Header.Set("Content-Type", "application/json")
	rec := serve(t, f.handler.Routes(), req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if f.appender.calls != 0 {
		t.Fatal("unsigned body must never reach the database")
	}
}

// Tampering after signing must fail even though the JSON is still equivalent.
func TestBodyTamperedAfterSigningIs401(t *testing.T) {
	f := newFixture(nil, nil)
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(inboundBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(verify.SignatureHeader, verify.Sign(appSecret, inboundBody))

	// Same signature, different bytes on the wire.
	tampered := bytes.Replace(inboundBody, []byte("hello"), []byte("HELLO"), 1)
	req.Body = io.NopCloser(bytes.NewReader(tampered))

	rec := serve(t, f.handler.Routes(), req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if f.appender.calls != 0 {
		t.Fatal("tampered body must not be stored")
	}
}

// Meta retries by resending identical bytes, so the dedupe key must be stable
// across retries and differ between genuinely different events.
func TestDeliveryKeyIsStableAcrossRetriesAndDistinctAcrossEvents(t *testing.T) {
	f := newFixture(nil, nil)
	h := f.handler.Routes()

	for i := 0; i < 3; i++ {
		rec := post(t, h, webhookPath, inboundBody, appSecret)
		if rec.Code != http.StatusOK {
			t.Fatalf("retry %d: want 200, got %d", i, rec.Code)
		}
	}
	for i := 1; i < len(f.appender.gotKeys); i++ {
		if f.appender.gotKeys[i] != f.appender.gotKeys[0] {
			t.Fatalf("retry %d produced a different delivery key: %s vs %s",
				i, f.appender.gotKeys[i], f.appender.gotKeys[0])
		}
	}

	other := bytes.Replace(inboundBody, []byte("wamid.HBgLMTY1"), []byte("wamid.OTHER"), 1)
	post(t, h, webhookPath, other, appSecret)
	if last := f.appender.gotKeys[len(f.appender.gotKeys)-1]; last == f.appender.gotKeys[0] {
		t.Fatal("different events must not collide on the delivery key")
	}
}

func TestDuplicateOutcomeIsAcknowledged(t *testing.T) {
	f := newFixture(nil, nil)
	f.appender.outcome = OutcomeDuplicate
	rec := post(t, f.handler.Routes(), webhookPath, inboundBody, appSecret)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an already-stored delivery, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), string(OutcomeDuplicate)) {
		t.Fatalf("want duplicate outcome reported, got %s", rec.Body)
	}
}

// An authenticated but unmodellable body is stored and acknowledged. Retrying
// malformed JSON forever is the failure mode this avoids.
func TestAuthenticatedGarbageIsStoredNotRetriedForever(t *testing.T) {
	f := newFixture(nil, nil)
	garbage := []byte(`this is not json at all`)

	rec := post(t, f.handler.Routes(), webhookPath, garbage, appSecret)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for authenticated garbage, got %d", rec.Code)
	}
	if f.appender.calls != 1 {
		t.Fatalf("want it stored for audit, got %d appends", f.appender.calls)
	}
	if !bytes.Equal(f.appender.gotBytes[0], garbage) {
		t.Fatal("raw unparseable body should still be preserved verbatim")
	}
}

func TestEmptyBodyIs400(t *testing.T) {
	f := newFixture(nil, nil)
	rec := post(t, f.handler.Routes(), webhookPath, []byte{}, appSecret)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	if f.appender.calls != 0 {
		t.Fatal("empty body must not be stored")
	}
}

func TestOversizeBodyIs413(t *testing.T) {
	f := newFixture(nil, nil)
	f.handler = New(Options{
		Resolve:      f.resolver,
		Append:       f.appender,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes: 64,
	})

	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'a'
	}
	rec := post(t, f.handler.Routes(), webhookPath, big, appSecret)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d", rec.Code)
	}
	if f.appender.calls != 0 {
		t.Fatal("oversize body must not be stored")
	}
}

func TestWrongMethodIs405(t *testing.T) {
	f := newFixture(nil, nil)
	req := httptest.NewRequest(http.MethodGet, webhookPath, nil)
	rec := serve(t, f.handler.Routes(), req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := serve(t, f.handler.Routes(), req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rec.Code)
	}
	if f.resolver.calls != 0 {
		t.Fatal("health check must not touch the secret resolver")
	}
}

func TestAttemptHeaderIsRecorded(t *testing.T) {
	for header, want := range map[string]int{"": 1, "3": 3, "1": 1, "abc": 1, "99999": 1} {
		f := newFixture(nil, nil)
		req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(inboundBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(verify.SignatureHeader, verify.Sign(appSecret, inboundBody))
		if header != "" {
			req.Header.Set("X-Delivery-Attempt", header)
		}
		if rec := serve(t, f.handler.Routes(), req); rec.Code != http.StatusOK {
			t.Fatalf("header %q: want 200, got %d", header, rec.Code)
		}
		if got := attemptFrom(req); got != want {
			t.Fatalf("header %q: want attempt %d, got %d", header, want, got)
		}
	}
}
