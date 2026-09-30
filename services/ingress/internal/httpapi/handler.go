// Package httpapi is the public edge of the ingress service.
//
// One rule governs every status code here: Meta stops retrying a webhook as
// soon as it sees 2xx. So a 200 means "durably stored, or provably already
// stored", and nothing else. Any outcome we cannot yet vouch for must be a
// 5xx, which makes Meta redeliver. Returning 200 optimistically would turn a
// database blip into permanent, invisible message loss.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/aucxley/chipmank/services/ingress/internal/metaevent"
	"github.com/aucxley/chipmank/services/ingress/internal/verify"
)

// DefaultMaxBodyBytes matches the largest payload Meta documents. Bounding the
// read is what keeps an unauthenticated caller from making us allocate.
const DefaultMaxBodyBytes = 8 << 20

var (
	// ErrUnknownApp means no channel account is configured for this app id.
	ErrUnknownApp = errors.New("httpapi: unknown provider app")
	// ErrUnknownChannel means the app is known but this phone number id is not
	// bound to a tenant channel account.
	ErrUnknownChannel = errors.New("httpapi: unknown channel account")
)

// Credential is a provider app secret together with a reference to where it
// was loaded from.
//
// The reference travels with the delivery and is persisted, because an
// ingress_items row that records "this body was verified" is only auditable if
// it also records which stored credential performed the verification.
type Credential struct {
	Secret []byte
	Ref    string
}

// SecretResolver maps a provider app id to its app credential.
//
// Resolution happens from the request path, never from the unverified body.
// An attacker naming someone else's app id only ever causes that app's
// credential to be used for a comparison that then fails.
type SecretResolver interface {
	AppCredential(ctx context.Context, providerAppID string) (Credential, error)
}

// Delivery is a verified, authenticated webhook ready to be persisted.
type Delivery struct {
	ProviderAppID string
	WABAID        string
	PhoneNumberID string
	DeliveryKey   string
	Signature     string
	AppSecretRef  string
	RawPayload    []byte
	Items         []metaevent.Item
	Attempt       int
	ReceivedAt    time.Time
}

// Outcome records what the appender decided about a delivery.
type Outcome string

const (
	// OutcomeStored means this is the first time we saw these bytes.
	OutcomeStored Outcome = "stored"
	// OutcomeDuplicate means an identical body is already durable. Retries are
	// expected and are acknowledged rather than re-projected.
	OutcomeDuplicate Outcome = "duplicate"
)

// Appender durably records a delivery.
//
// The interface exists so the Postgres implementation and the in-memory test
// double share one contract. Implementations must only return nil once the
// data is committed.
type Appender interface {
	Append(ctx context.Context, d Delivery) (Outcome, error)
}

type Options struct {
	Resolve      SecretResolver
	Append       Appender
	Logger       *slog.Logger
	MaxBodyBytes int64
	Now          func() time.Time
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return o
}

// Handler serves POST /webhooks/meta/{provider_app_id}.
type Handler struct {
	opt Options
}

func New(opt Options) *Handler {
	return &Handler{opt: opt.withDefaults()}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /webhooks/meta/{provider_app_id}", h.serveWebhook)
	return mux
}

func (h *Handler) serveWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	providerAppID := r.PathValue("provider_app_id")
	log := h.opt.Logger.With("provider_app_id", providerAppID)

	if providerAppID == "" {
		h.fail(w, http.StatusNotFound, "missing_provider_app_id")
		return
	}

	cred, err := h.opt.Resolve.AppCredential(ctx, providerAppID)
	if errors.Is(err, ErrUnknownApp) {
		// Not an error condition. 404 stops Meta retrying an endpoint we were
		// never configured for, and reveals nothing about which ids exist.
		h.fail(w, http.StatusNotFound, "unknown_app")
		return
	}
	if err != nil {
		log.Error("resolve app credential", "err", err)
		h.fail(w, http.StatusServiceUnavailable, "secret_lookup_failed")
		return
	}

	body, ok := h.readBody(w, r, log)
	if !ok {
		return
	}

	// Verify against the exact bytes read off the wire. No re-serialisation
	// may happen between here and Append.
	if err := verify.Verify(cred.Secret, body, r.Header.Get(verify.SignatureHeader)); err != nil {
		switch {
		case errors.Is(err, verify.ErrMissingSignature),
			errors.Is(err, verify.ErrMalformed),
			errors.Is(err, verify.ErrLegacyAlgorithm):
			log.Warn("webhook signature not acceptable", "err", err)
		default:
			// A mismatch against a known app is the signature that matters.
			log.Error("webhook signature mismatch", "err", err)
		}
		// Nothing is stored: an unauthenticated body must never reach the DB.
		h.fail(w, http.StatusUnauthorized, "signature_invalid")
		return
	}

	_, items, perr := metaevent.Parse(body)
	if perr != nil {
		// The sender is authenticated but the payload is not JSON we model.
		// Persist it as a rejection so it is auditable and never retried,
		// then acknowledge.
		log.Warn("authenticated payload not parseable", "err", perr)
		items = nil
	}

	delivery := Delivery{
		ProviderAppID: providerAppID,
		DeliveryKey:   metaevent.DeliveryKey(providerAppID, body),
		Signature:     r.Header.Get(verify.SignatureHeader),
		AppSecretRef:  cred.Ref,
		RawPayload:    body,
		Items:         items,
		Attempt:       attemptFrom(r),
		ReceivedAt:    h.opt.Now().UTC(),
	}
	for _, it := range items {
		if delivery.WABAID == "" {
			delivery.WABAID = it.WABAID
		}
		if delivery.PhoneNumberID == "" {
			delivery.PhoneNumberID = it.PhoneNumberID
		}
	}

	outcome, err := h.opt.Append.Append(ctx, delivery)
	if err != nil {
		// The one case where 5xx is the correct answer. Meta will redeliver
		// and the dedupe key makes the retry harmless.
		log.Error("durable append failed", "err", err, "delivery_key", delivery.DeliveryKey)
		h.fail(w, http.StatusServiceUnavailable, "append_failed")
		return
	}

	log.Info("webhook durably stored",
		"delivery_key", delivery.DeliveryKey,
		"outcome", outcome,
		"items", len(delivery.Items),
		"phone_number_id", delivery.PhoneNumberID,
	)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":       string(outcome),
		"delivery_key": delivery.DeliveryKey,
	})
}

// readBody returns the raw bytes, or false if it already wrote an error.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, log *slog.Logger) ([]byte, bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" && ct != "application/json" {
		h.fail(w, http.StatusUnsupportedMediaType, "unsupported_content_type")
		return nil, false
	}

	limited := http.MaxBytesReader(w, r.Body, h.opt.MaxBodyBytes)
	body := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for {
		n, err := limited.Read(buf)
		body = append(body, buf[:n]...)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				h.fail(w, http.StatusRequestEntityTooLarge, "body_too_large")
				return nil, false
			}
			if errors.Is(err, io.EOF) {
				break
			}
			log.Warn("read request body", "err", err)
			h.fail(w, http.StatusBadRequest, "body_read_failed")
			return nil, false
		}
	}
	if len(body) == 0 {
		h.fail(w, http.StatusBadRequest, "empty_body")
		return nil, false
	}
	return body, true
}

func attemptFrom(r *http.Request) int {
	// Meta does not send an explicit attempt counter, but proxies often add
	// one. Recorded as a hint only; the dedupe key is what enforces at-most-once.
	if v := r.Header.Get("X-Delivery-Attempt"); v != "" {
		var n int
		for _, c := range v {
			if c < '0' || c > '9' {
				return 1
			}
			n = n*10 + int(c-'0')
			if n > 1000 {
				return 1
			}
		}
		return n
	}
	return 1
}

func (h *Handler) fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  code,
		"status": status,
	})
}
