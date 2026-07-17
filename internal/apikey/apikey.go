// Package apikey generates and verifies REST API access tokens for Paper Plane.
//
// A token is a high-entropy (256-bit) random string with a fixed "pp_" prefix.
// Because the token carries far more entropy than any password, it does not need
// a slow hash like bcrypt: only its SHA-256 hash is stored. Authentication hashes
// the presented token and looks the digest up directly (an exact, indexed match).
// A timing side-channel on that lookup is not exploitable: the lookup key is a
// SHA-256 digest, so preimage resistance means an attacker cannot turn any leaked
// timing into a token that hashes to a stored value. The plaintext token is
// returned to the caller exactly once (at generation) and never persisted.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Prefix is prepended to every generated token so keys are recognizable in logs
// and configuration (and so an accidental empty value never validates).
const Prefix = "pp_"

// tokenBytes is the number of random bytes in a token body. 32 bytes → 256 bits
// of entropy, encoded as 64 hex characters.
const tokenBytes = 32

// Generate returns a new plaintext token (with Prefix) and its hex-encoded
// SHA-256 hash. Store only the hash; show the plaintext to the operator once.
func Generate() (token, hash string) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read never returns an error on supported platforms; panic
		// rather than silently emit a low-entropy token.
		panic("apikey: crypto/rand failed: " + err.Error())
	}
	token = Prefix + hex.EncodeToString(buf)
	return token, Hash(token)
}

// Hash returns the hex-encoded SHA-256 of a plaintext token. It is used both to
// derive the stored hash at creation and to look up a presented token.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// FromAuthorizationHeader extracts the bearer token from an Authorization header
// value. It accepts "Bearer <token>" (case-insensitive scheme) and returns ""
// when the header is empty, malformed, or carries a non-bearer scheme.
func FromAuthorizationHeader(header string) string {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(header[len(scheme):])
}
