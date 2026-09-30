// Package postgres is the durable implementation of the ingress contracts.
//
// The append is a single transaction. Either the delivery rows are committed
// and the handler may answer 200, or nothing is written and the handler must
// answer 5xx so Meta redelivers. There is no code path that acknowledges an
// event which is not on disk.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aucxley/chipmank/services/ingress/internal/httpapi"
	"github.com/aucxley/chipmank/services/ingress/internal/metaevent"
)

// Store is the durable Appender.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Append writes a verified delivery to ingress_items.
//
// It runs as app_ingress, which may insert into the acceptance tables and read
// the routing table, but cannot read any tenant business data. Dedupe is
// enforced by the UNIQUE (provider_app_id, item_fingerprint) index rather than
// by a prior SELECT, so two concurrent redeliveries cannot both insert.
func (s *Store) Append(ctx context.Context, d httpapi.Delivery) (httpapi.Outcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("postgres: begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe on both
	// paths and means an early return can never leave the tx open.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_ingress`); err != nil {
		return "", fmt.Errorf("postgres: set role: %w", err)
	}

	tenantID, channelAccountID, epoch, err := resolveRouting(ctx, tx, d)
	if err != nil {
		return "", err
	}

	fingerprints := itemFingerprints(d)
	stored, duplicates := 0, 0

	var phoneNumberID, wabaID *string
	if d.PhoneNumberID != "" {
		phoneNumberID = &d.PhoneNumberID
	}
	if d.WABAID != "" {
		wabaID = &d.WABAID
	}

	for i, fp := range fingerprints {
		// Dedupe is done by catching the unique violation, not with
		// ON CONFLICT DO NOTHING.
		//
		// ON CONFLICT needs SELECT privilege on the table even for DO NOTHING,
		// and it additionally needs an applicable SELECT row-level policy.
		// Granting either to the ingest role would hand it the ability to read
		// raw inbound message bodies, which is precisely what this role is
		// meant to be unable to do. The unique index does the real work either
		// way, so two concurrent redeliveries still cannot both insert.
		//
		// The savepoint confines the abort: a duplicate insert poisons the
		// transaction it runs in, and there are more items to write in this same
		// transaction. The savepoint statements are issued directly rather than
		// through pgx's nested-transaction helper, because that helper leaves
		// the outer transaction unusable once a statement inside it fails.
		sp := fmt.Sprintf("ingress_item_%d", i)

		if _, err := tx.Exec(ctx, "SAVEPOINT "+sp); err != nil {
			return "", fmt.Errorf("postgres: savepoint: %w", err)
		}

		// signature_verified is true because the handler only ever calls Append
		// after Verify returned nil. Storing the raw payload means a later
		// re-verification can still reproduce the original HMAC.
		_, err := tx.Exec(ctx, `
			INSERT INTO ingress_items (
				provider_app_id, item_fingerprint, phone_number_id, waba_id,
				payload, signature_header, app_secret_ref, signature_verified,
				tenant_id, channel_account_id, placement_epoch, received_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, true, $8, $9, $10, $11)
		`, d.ProviderAppID, fp, phoneNumberID, wabaID,
			d.RawPayload, d.Signature, d.AppSecretRef,
			tenantID, channelAccountID, epoch, d.ReceivedAt)

		if err != nil {
			if _, rbErr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+sp); rbErr != nil {
				return "", fmt.Errorf("postgres: rollback to savepoint after %v: %w", err, rbErr)
			}
			if _, relErr := tx.Exec(ctx, "RELEASE SAVEPOINT "+sp); relErr != nil {
				return "", fmt.Errorf("postgres: release savepoint: %w", relErr)
			}
			if isUniqueViolation(err) {
				duplicates++
				continue
			}
			return "", fmt.Errorf("postgres: insert ingress item: %w", err)
		}

		if _, err := tx.Exec(ctx, "RELEASE SAVEPOINT "+sp); err != nil {
			return "", fmt.Errorf("postgres: release savepoint: %w", err)
		}
		stored++
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("postgres: commit: %w", err)
	}

	if stored == 0 {
		return httpapi.OutcomeDuplicate, nil
	}
	return httpapi.OutcomeStored, nil
}

// isUniqueViolation reports whether err is a unique constraint violation.
//
// This is the dedupe signal. It is checked against the SQLSTATE code rather
// than the constraint name so that adding another unique index later does not
// silently change which errors are treated as "already stored".
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// itemFingerprints yields one identity per addressable item, or a single
// delivery-level identity when nothing could be parsed. An authenticated body
// with no recognisable items is still stored: dropping it would make the
// rejection untraceable, and refusing to acknowledge it would make Meta retry
// unparseable bytes forever.
func itemFingerprints(d httpapi.Delivery) []string {
	if len(d.Items) == 0 {
		return []string{metaevent.UnparsedFingerprint(d.ProviderAppID, d.DeliveryKey)}
	}
	out := make([]string, 0, len(d.Items))
	for _, it := range d.Items {
		out = append(out, metaevent.ItemFingerprint(d.ProviderAppID, it))
	}
	return out
}

// resolveRouting maps the provider's phone number to a tenant BEFORE any
// conversation exists. A miss is not an error: the item is still stored with a
// null tenant and surfaced as needing operator attention.
func resolveRouting(ctx context.Context, tx pgx.Tx, d httpapi.Delivery) (tenantID, channelAccountID *uuid.UUID, epoch *int64, err error) {
	if d.PhoneNumberID == "" {
		return nil, nil, nil, nil
	}

	var (
		tID uuid.UUID
		cID uuid.UUID
		ep  int64
	)
	err = tx.QueryRow(ctx, `
		SELECT tenant_id, channel_account_id, placement_epoch
		  FROM tenant_routing_bindings
		 WHERE provider_app_id = $1 AND phone_number_id = $2
	`, d.ProviderAppID, d.PhoneNumberID).Scan(&tID, &cID, &ep)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("postgres: resolve routing: %w", err)
	}
	return &tID, &cID, &ep, nil
}

// CredentialResolver resolves app secrets from channel_accounts.credential_ref
// and loads the actual secret from the environment.
//
// The database never holds the secret itself (v2 section 30); it holds a
// reference like "env:META_APP_SECRET_PROD", and the value is read from the
// process environment. That keeps this honest for local work while leaving a
// single place to change for a real secret manager.
type CredentialResolver struct {
	pool *pgxpool.Pool
	// override supplies secrets directly, bypassing the reference indirection.
	// Used by tests and by local development.
	override map[string][]byte
}

func NewCredentialResolver(pool *pgxpool.Pool, override map[string][]byte) *CredentialResolver {
	return &CredentialResolver{pool: pool, override: override}
}

func (r *CredentialResolver) AppCredential(ctx context.Context, providerAppID string) (httpapi.Credential, error) {
	if secret, ok := r.override[providerAppID]; ok {
		return httpapi.Credential{Secret: secret, Ref: "override:" + providerAppID}, nil
	}

	var ref *string
	err := r.pool.QueryRow(ctx, `
		SELECT credential_ref
		  FROM channel_accounts
		 WHERE provider_app_id = $1 AND credential_ref IS NOT NULL
		 LIMIT 1
	`, providerAppID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return httpapi.Credential{}, httpapi.ErrUnknownApp
	}
	if err != nil {
		return httpapi.Credential{}, fmt.Errorf("postgres: lookup credential_ref: %w", err)
	}

	secret, err := loadSecret(*ref)
	if err != nil {
		return httpapi.Credential{}, err
	}
	return httpapi.Credential{Secret: secret, Ref: *ref}, nil
}

// loadSecret understands "env:NAME". Other schemes are rejected rather than
// silently ignored, so an unsupported reference cannot be mistaken for a
// working configuration.
func loadSecret(ref string) ([]byte, error) {
	name, ok := strings.CutPrefix(ref, "env:")
	if !ok {
		return nil, fmt.Errorf("postgres: unsupported credential_ref scheme %q; only env: is implemented", ref)
	}
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		return nil, fmt.Errorf("postgres: credential_ref %q points at unset variable %s", ref, name)
	}
	return []byte(v), nil
}
