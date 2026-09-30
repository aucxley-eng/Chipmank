package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aucxley/chipmank/services/ingress/internal/httpapi"
	"github.com/aucxley/chipmank/services/ingress/internal/metaevent"
	"github.com/aucxley/chipmank/services/ingress/internal/store/postgres"
	"github.com/aucxley/chipmank/services/ingress/internal/verify"
)

// These tests run against a real Postgres, not a double. The default target is
// the wire-protocol PGlite server started by tools/dbtest/serve.mjs, so the
// append path is exercised with real SQL, real roles and real row-level
// security. Point CHIPMANK_TEST_DATABASE_URL at a real Postgres to run them
// there instead; the assertions are identical.
const defaultDSN = "postgres://postgres@127.0.0.1:55432/postgres"

const (
	appID       = "dev-app-1"
	phoneNumber = "pn-dev-1"
	waba        = "waba-dev-1"
	tenantID    = "11111111-1111-1111-1111-111111111111"
	secret      = "integration-secret"
)

func dsn(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("CHIPMANK_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultDSN
}

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Skipf("no test database at %s: %v", dsn(t), err)
	}
	// Simple protocol. pgx otherwise caches prepared statements by name, and
	// the PGlite socket server cannot serve a repeated Parse of the same name
	// across connections. This is a limitation of the in-process test harness,
	// not of Postgres: against a real server the default cached mode is fine
	// and is what production uses.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("no test database at %s: %v", dsn(t), err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		t.Skipf("no test database at %s: %v", dsn(t), err)
	}
	t.Cleanup(p.Close)
	return p
}

// ingressBody is a realistic inbound text message. Built with the same
// metaevent types the server uses, so the harness cannot drift from the shape
// the code actually parses.
func ingressBody(t *testing.T, wamid string) []byte {
	t.Helper()
	env := metaevent.Envelope{
		Object: "whatsapp_business_account",
		Entry: []metaevent.Entry{{
			ID: waba,
			Changes: []metaevent.Change{{
				Field: "messages",
				Value: mustJSON(t, metaevent.Value{
					MessagingProduct: "whatsapp",
					Metadata: metaevent.Metadata{
						DisplayPhoneNumber: "15550001111",
						PhoneNumberID:      phoneNumber,
					},
					Messages: []metaevent.Message{{
						From: "15551230000", ID: wamid,
						Timestamp: "1735689600", Type: "text",
						Text: &metaevent.TextMessage{Body: "durable hello"},
					}},
				}),
			}},
		}},
	}
	return mustJSON(t, env)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

type harness struct {
	handler *httpapi.Handler
	pool    *pgxpool.Pool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p := pool(t)
	store := postgres.NewStore(p)
	resolver := postgres.NewCredentialResolver(p, nil)
	t.Setenv("META_APP_SECRET", secret)

	return &harness{
		pool:    p,
		handler: httpapi.New(httpapi.Options{Resolve: resolver, Append: store}),
	}
}

func (h *harness) post(t *testing.T, body []byte, signWith string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/meta/"+appID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(verify.SignatureHeader, verify.Sign([]byte(signWith), body))
	rec := httptest.NewRecorder()
	h.handler.Routes().ServeHTTP(rec, req)
	return rec
}

// countRows reads through the migration superuser, because app_ingress cannot
// read ingress_items at all. That inability is asserted separately.
func (h *harness) countRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ingress_items WHERE provider_app_id = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count ingress_items: %v", err)
	}
	return n
}

// ---------------------------------------------------------- the durable test

// The central claim of the whole service: a 200 means the raw body is on disk,
// byte for byte, with the HMAC still reproducible from the stored bytes.
func TestAcknowledgedWebhookIsDurableAndByteExact(t *testing.T) {
	h := newHarness(t)
	body := ingressBody(t, "wamid.DURABLE1")

	rec := h.post(t, body, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body)
	}

	ctx := context.Background()
	var (
		payload     []byte
		sigHeader   string
		credRef     string
		verified    bool
		gotTenant   *string
		gotPhone    *string
		gotWABA     *string
		committedAt int64
	)
	err := h.pool.QueryRow(ctx, `
		SELECT payload, signature_header, app_secret_ref, signature_verified,
		       tenant_id::text, phone_number_id, waba_id, committed_position
		  FROM ingress_items
		 WHERE provider_app_id = $1
		   AND payload = $2
	`, appID, body).Scan(&payload, &sigHeader, &credRef, &verified,
		&gotTenant, &gotPhone, &gotWABA, &committedAt)
	if err != nil {
		t.Fatalf("stored row not found: %v", err)
	}

	if !bytes.Equal(payload, body) {
		t.Fatalf("stored payload differs from the signed bytes\n got: %s\nwant: %s", payload, body)
	}
	// Re-verify from what is on disk. If this passes, the stored bytes are
	// exactly the bytes Meta signed, not a re-serialisation of them.
	if err := verify.Verify([]byte(secret), payload, sigHeader); err != nil {
		t.Fatalf("stored bytes do not reproduce the signature: %v", err)
	}
	if !verified {
		t.Fatal("row must be marked signature_verified")
	}
	if credRef != "env:META_APP_SECRET" {
		t.Fatalf("want the credential reference recorded for audit, got %q", credRef)
	}
	if gotTenant == nil || *gotTenant != tenantID {
		t.Fatalf("want tenant %s resolved from routing, got %v", tenantID, gotTenant)
	}
	if gotPhone == nil || *gotPhone != phoneNumber {
		t.Fatalf("want phone_number_id %s, got %v", phoneNumber, gotPhone)
	}
	if gotWABA == nil || *gotWABA != waba {
		t.Fatalf("want waba_id %s, got %v", waba, gotWABA)
	}
	if committedAt == 0 {
		t.Fatal("want a committed log position, got 0")
	}
}

// A retry of the identical body must not create a second row.
func TestRetryIsDeduplicatedNotDuplicated(t *testing.T) {
	h := newHarness(t)
	body := ingressBody(t, "wamid.RETRY1")

	before := h.countRows(t)
	first := h.post(t, body, secret)
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery: want 200, got %d: %s", first.Code, first.Body)
	}
	second := h.post(t, body, secret)
	if second.Code != http.StatusOK {
		t.Fatalf("retry: want 200, got %d: %s", second.Code, second.Body)
	}

	if got := h.countRows(t) - before; got != 1 {
		t.Fatalf("want exactly 1 row for 2 identical deliveries, got %d", got)
	}

	var firstKey, secondKey string
	_ = json.Unmarshal(first.Body.Bytes(), &struct {
		DeliveryKey *string `json:"delivery_key"`
	}{DeliveryKey: &firstKey})
	_ = json.Unmarshal(second.Body.Bytes(), &struct {
		DeliveryKey *string `json:"delivery_key"`
	}{DeliveryKey: &secondKey})
	if firstKey == "" || firstKey != secondKey {
		t.Fatalf("want a stable delivery key across retries, got %q then %q", firstKey, secondKey)
	}
}

// Two distinct events must both be stored.
func TestDistinctEventsBothPersist(t *testing.T) {
	h := newHarness(t)
	before := h.countRows(t)

	for _, id := range []string{"wamid.DISTINCT1", "wamid.DISTINCT2"} {
		if rec := h.post(t, ingressBody(t, id), secret); rec.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", id, rec.Code)
		}
	}
	if got := h.countRows(t) - before; got != 2 {
		t.Fatalf("want 2 rows for 2 distinct events, got %d", got)
	}
}

// An unauthenticated body must leave no trace at all.
func TestUnauthenticatedBodyLeavesNoRow(t *testing.T) {
	h := newHarness(t)
	before := h.countRows(t)

	rec := h.post(t, ingressBody(t, "wamid.BAD1"), "the-wrong-secret")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if got := h.countRows(t); got != before {
		t.Fatalf("unauthenticated body wrote %d rows", got-before)
	}
}

// An unknown phone number is still stored, with no tenant. Dropping it would
// lose the event; refusing to acknowledge it would make Meta retry forever.
func TestUnroutableNumberIsStoredWithoutTenant(t *testing.T) {
	h := newHarness(t)
	body := bytes.Replace(ingressBody(t, "wamid.UNROUTED"), []byte(phoneNumber), []byte("pn-unknown"), -1)
	before := h.countRows(t)

	rec := h.post(t, body, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an unroutable but authentic body, got %d", rec.Code)
	}

	var tenant *string
	err := h.pool.QueryRow(context.Background(), `
		SELECT tenant_id::text FROM ingress_items
		 WHERE provider_app_id = $1 AND payload = $2`, appID, body).Scan(&tenant)
	if err != nil {
		t.Fatalf("unroutable item was not stored: %v", err)
	}
	if tenant != nil {
		t.Fatalf("want a null tenant for an unroutable number, got %v", *tenant)
	}
	if got := h.countRows(t) - before; got != 1 {
		t.Fatalf("want 1 row, got %d", got)
	}
}

// The ingest role must not be able to read raw bodies back, even though it
// wrote them. This is asserted here as well as in the schema suite because it
// is the guarantee the role design exists to provide.
func TestIngestRoleCannotReadBackWhatItWrote(t *testing.T) {
	h := newHarness(t)
	if rec := h.post(t, ingressBody(t, "wamid.NOREAD1"), secret); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	ctx := context.Background()
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_ingress`); err != nil {
		t.Fatalf("set role: %v", err)
	}

	var canSelect bool
	if err := tx.QueryRow(ctx,
		`SELECT has_table_privilege('app_ingress','ingress_items','SELECT')`).Scan(&canSelect); err != nil {
		t.Fatalf("privilege probe: %v", err)
	}
	if canSelect {
		t.Fatal("app_ingress holds SELECT on ingress_items; raw bodies are reachable")
	}

	// The guarantee is that no raw body is readable. How that is enforced is
	// incidental: the privilege denial normally surfaces when the result set is
	// iterated rather than when the query is sent, so both outcomes are
	// acceptable here and only an actual payload is a failure.
	rows, qerr := tx.Query(ctx, `SELECT payload FROM ingress_items`)
	denied := false
	if qerr != nil {
		denied = isPermissionDenied(qerr)
		if !denied {
			t.Fatalf("unexpected error reading as app_ingress: %v", qerr)
		}
	} else {
		for rows.Next() {
			var payload []byte
			if err := rows.Scan(&payload); err != nil {
				t.Fatalf("scan: %v", err)
			}
			t.Fatalf("app_ingress read a raw inbound body (%d bytes)", len(payload))
		}
		if err := rows.Err(); err != nil {
			denied = isPermissionDenied(err)
			if !denied {
				t.Fatalf("unexpected error iterating as app_ingress: %v", err)
			}
		}
		rows.Close()
	}

	if !denied {
		// No error means privilege was present, so row-level security is the
		// only thing standing between the ingest role and raw bodies. Confirm
		// there is genuinely no SELECT policy for it.
		var policies int
		if err := h.pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_policies
			 WHERE tablename = 'ingress_items'
			   AND cmd = 'SELECT'
			   AND roles @> ARRAY['app_ingress']::name[]`).Scan(&policies); err != nil {
			t.Fatalf("policy probe: %v", err)
		}
		if policies != 0 {
			t.Fatalf("app_ingress can select rows it must not see: %d SELECT policies", policies)
		}
	}
}

func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42501"
	}
	return false
}

// The credential reference must resolve through the database path, not only the
// test override, so the wiring is proven for a real deployment.
func TestCredentialResolvesThroughDatabase(t *testing.T) {
	p := pool(t)
	t.Setenv("META_APP_SECRET", secret)
	r := postgres.NewCredentialResolver(p, nil)

	cred, err := r.AppCredential(context.Background(), appID)
	if err != nil {
		t.Fatalf("resolve credential from channel_accounts: %v", err)
	}
	if !bytes.Equal(cred.Secret, []byte(secret)) {
		t.Fatalf("want the secret from env:META_APP_SECRET, got %q", cred.Secret)
	}
	if cred.Ref != "env:META_APP_SECRET" {
		t.Fatalf("want the reference recorded, got %q", cred.Ref)
	}

	if _, err := r.AppCredential(context.Background(), "no-such-app"); err == nil {
		t.Fatal("want ErrUnknownApp for an unconfigured app")
	}
}

// The path on a stored row must be a real committed position that increases,
// since it is what a status receipt points back at.
func TestCommittedPositionIsMonotonic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var prev int64
	for i, id := range []string{"wamid.POS1", "wamid.POS2", "wamid.POS3"} {
		if rec := h.post(t, ingressBody(t, id), secret); rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: want 200, got %d", i, rec.Code)
		}
		var pos int64
		err := h.pool.QueryRow(ctx, `
			SELECT committed_position FROM ingress_items
			 WHERE provider_app_id = $1 AND payload = $2`, appID, ingressBody(t, id)).Scan(&pos)
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
		if pos <= prev {
			t.Fatalf("committed_position did not advance: got %d after %d", pos, prev)
		}
		prev = pos
	}
}
