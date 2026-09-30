// Package verify implements Meta Cloud API webhook signature checking.
//
// The signature covers the exact bytes Meta sent on the wire. Nothing here may
// re-serialise, pretty-print, normalise or decode the payload before hashing:
// a body that parses to identical JSON but differs by a single byte of
// whitespace is a different body, and must fail verification.
package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SignatureHeader carries the sha256 digest. LegacySignatureHeader carries a
// sha1 digest and is deliberately unsupported: sha1 is deprecated by Meta and
// accepting it would only widen the attack surface.
const (
	SignatureHeader       = "X-Hub-Signature-256"
	LegacySignatureHeader = "X-Hub-Signature"
)

const digestPrefix = "sha256="

var (
	ErrMissingSignature = errors.New("verify: signature header absent")
	ErrMalformed        = errors.New("verify: signature header malformed")
	ErrMismatch         = errors.New("verify: signature mismatch")
	ErrLegacyAlgorithm  = errors.New("verify: sha1 signature rejected")
)

// Sign produces the value Meta would put in SignatureHeader for body.
//
// It lives beside Verify on purpose: the simulator and the tests must sign
// bytes the same way production verifies them, otherwise the harness can drift
// into testing something the server never does.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return digestPrefix + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks header against the HMAC of body.
//
// Errors are split by cause so operators can tell "someone sent us an unsigned
// webhook" from "our secret does not match" — the second is an incident, the
// first is usually a misconfigured endpoint. The distinction is never used to
// change the HTTP response, only the log line and metric label.
func Verify(secret, body []byte, header string) error {
	if strings.TrimSpace(header) == "" {
		return ErrMissingSignature
	}

	// A sha1 digest in the legacy header, or in the modern header, is refused
	// before any comparison happens.
	if strings.HasPrefix(header, "sha1=") {
		return fmt.Errorf("%w: %s", ErrLegacyAlgorithm, LegacySignatureHeader)
	}

	raw, ok := strings.CutPrefix(header, digestPrefix)
	if !ok {
		return fmt.Errorf("%w: expected %q prefix", ErrMalformed, digestPrefix)
	}

	supplied, err := hex.DecodeString(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(body)

	// hmac.Equal is constant time. A plain bytes.Equal here would leak the
	// digest prefix through response timing.
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return ErrMismatch
	}
	return nil
}
