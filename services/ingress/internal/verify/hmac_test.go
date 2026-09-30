package verify

import (
	"errors"
	"strings"
	"testing"
)

var testSecret = []byte("test-app-secret")

func TestVerifyAcceptsCorrectSignature(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	if err := Verify(testSecret, body, Sign(testSecret, body)); err != nil {
		t.Fatalf("expected valid signature to verify, got %v", err)
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account"}`)
	err := Verify([]byte("attacker-secret"), body, Sign(testSecret, body))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected ErrMismatch, got %v", err)
	}
}

// The core invariant from v2 section 20: the digest covers the raw bytes. Any
// transformation of the body after it was signed must fail, even when the
// result still parses to the same JSON.
func TestVerifyRejectsBodyAlteredAfterSigning(t *testing.T) {
	signed := []byte(`{"object":"whatsapp_business_account"}`)
	header := Sign(testSecret, signed)

	for name, mutated := range map[string][]byte{
		"whitespace added": []byte(`{"object": "whatsapp_business_account"}`),
		"newline appended": append(append([]byte{}, signed...), '\n'),
		"key reordered":    []byte(`{"entry":[],"object":"whatsapp_business_account"}`),
		"truncated":        signed[:len(signed)-1],
		"empty":            {},
		"pretty printed":   []byte("{\n  \"object\": \"whatsapp_business_account\"\n}"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(testSecret, mutated, header); !errors.Is(err, ErrMismatch) {
				t.Fatalf("expected ErrMismatch for %s, got %v", name, err)
			}
		})
	}
}

func TestVerifyRejectsTruncatedDigest(t *testing.T) {
	body := []byte(`{"object":"x"}`)
	full := Sign(testSecret, body)
	// A prefix of a valid digest must not pass.
	truncated := full[:len(full)-2]
	if err := Verify(testSecret, body, truncated); !errors.Is(err, ErrMismatch) {
		t.Fatalf("expected truncated digest to fail, got %v", err)
	}
}

func TestVerifyRejectsMissingHeader(t *testing.T) {
	for name, header := range map[string]string{"empty": "", "whitespace": "   "} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(testSecret, []byte(`{}`), header); !errors.Is(err, ErrMissingSignature) {
				t.Fatalf("expected ErrMissingSignature, got %v", err)
			}
		})
	}
}

func TestVerifyRejectsMalformedHeader(t *testing.T) {
	for name, header := range map[string]string{
		"no prefix":    "abcdef0123456789",
		"wrong prefix": "sha512=abcdef",
		"bad hex":      "sha256=zzzz",
		"odd length":   "sha256=abc",
	} {
		t.Run(name, func(t *testing.T) {
			err := Verify(testSecret, []byte(`{}`), header)
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("expected ErrMalformed for %q, got %v", header, err)
			}
		})
	}
}

// Hex is case-insensitive on decode, so an uppercased digest carries the same
// bytes and must still authenticate. Transport casing is not a security
// boundary; what matters is that the decoded bytes match.
func TestVerifyAcceptsUppercaseHexDigest(t *testing.T) {
	body := []byte(`{"object":"x"}`)
	upper := "sha256=" + strings.ToUpper(Sign(testSecret, body)[len(digestPrefix):])
	if err := Verify(testSecret, body, upper); err != nil {
		t.Fatalf("expected uppercase digest to verify, got %v", err)
	}
}

func TestVerifyRejectsLegacySHA1(t *testing.T) {
	err := Verify(testSecret, []byte(`{}`), "sha1=07e4e4d1c2b6a1f2c9d8e7f6a5b4c3d2e1f0a9b8")
	if !errors.Is(err, ErrLegacyAlgorithm) {
		t.Fatalf("expected ErrLegacyAlgorithm, got %v", err)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	body := []byte(`{"object":"x"}`)
	a, b := Sign(testSecret, body), Sign(testSecret, body)
	if a != b {
		t.Fatalf("Sign not deterministic: %s vs %s", a, b)
	}
	if len(a) != len(digestPrefix)+sha256HexLen {
		t.Fatalf("unexpected signature length %d", len(a))
	}
}

const sha256HexLen = 64
