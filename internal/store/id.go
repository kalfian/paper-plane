package store

import (
	"crypto/rand"
)

// idAlphabet is a URL-safe, unambiguous alphabet for generated IDs. It uses the
// standard nanoid-style set (A-Za-z0-9_-) which is safe both in URLs and as a
// filesystem folder name.
const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz_-"

// defaultIDSize is the number of characters in a generated project ID.
const defaultIDSize = 12

// NewID returns a cryptographically random, URL-safe identifier of
// defaultIDSize characters.
func NewID() string {
	return newIDN(defaultIDSize)
}

// newIDN returns a random URL-safe id of exactly n characters. Because
// len(idAlphabet) is 64 (a power of two), each 6-bit chunk maps to exactly one
// symbol with no modulo bias.
func newIDN(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read never returns an error on supported platforms; panic
		// rather than silently emit a low-entropy ID.
		panic("store: crypto/rand failed: " + err.Error())
	}
	for i := range buf {
		buf[i] = idAlphabet[buf[i]&0x3F]
	}
	return string(buf)
}
