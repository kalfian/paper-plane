package apikey

import (
	"strings"
	"testing"
)

func TestGenerateProducesPrefixedTokenAndMatchingHash(t *testing.T) {
	token, hash := Generate()

	if !strings.HasPrefix(token, Prefix) {
		t.Fatalf("token %q missing prefix %q", token, Prefix)
	}
	// Prefix + 64 hex chars (32 bytes).
	if want := len(Prefix) + 2*tokenBytes; len(token) != want {
		t.Fatalf("token length = %d, want %d", len(token), want)
	}
	if hash != Hash(token) {
		t.Fatal("returned hash does not match Hash(token)")
	}
	// Hash is 64 hex chars (SHA-256).
	if len(hash) != 64 {
		t.Fatalf("hash length = %d, want 64", len(hash))
	}
}

func TestGenerateIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		token, hash := Generate()
		if seen[token] {
			t.Fatal("duplicate token generated")
		}
		if seen[hash] {
			t.Fatal("duplicate hash generated")
		}
		seen[token] = true
		seen[hash] = true
	}
}

func TestFromAuthorizationHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"standard", "Bearer pp_abc", "pp_abc"},
		{"lowercase scheme", "bearer pp_abc", "pp_abc"},
		{"mixed case scheme", "BeArEr pp_abc", "pp_abc"},
		{"trailing space trimmed", "Bearer   pp_abc  ", "pp_abc"},
		{"empty", "", ""},
		{"no scheme", "pp_abc", ""},
		{"wrong scheme", "Basic pp_abc", ""},
		{"scheme only", "Bearer ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FromAuthorizationHeader(tc.header); got != tc.want {
				t.Fatalf("FromAuthorizationHeader(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}
