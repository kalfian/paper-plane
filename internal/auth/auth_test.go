package auth

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func testManager() *Manager {
	return NewManager([]byte("test-secret-0123456789abcdef"), false)
}

func TestPasswordHashVerify(t *testing.T) {
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse") {
		t.Fatal("VerifyPassword: correct password rejected")
	}
	if VerifyPassword(hash, "wrong") {
		t.Fatal("VerifyPassword: wrong password accepted")
	}
	if VerifyPassword("not-a-hash", "correct horse") {
		t.Fatal("VerifyPassword: malformed hash accepted")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	m := testManager()
	c := m.IssueSessionCookie()

	if !c.HttpOnly {
		t.Error("session cookie not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie SameSite != Lax")
	}
	if c.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", c.Path)
	}

	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	if !m.VerifySessionRequest(r) {
		t.Fatal("valid session cookie rejected")
	}
}

func TestSessionNoCookie(t *testing.T) {
	m := testManager()
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	if m.VerifySessionRequest(r) {
		t.Fatal("request without cookie accepted")
	}
}

func TestSessionTampered(t *testing.T) {
	m := testManager()
	c := m.IssueSessionCookie()

	tests := []struct {
		name  string
		value string
	}{
		{"flipped payload char", flipFirstChar(c.Value)},
		{"truncated", c.Value[:len(c.Value)-2]},
		{"no dot", strings.ReplaceAll(c.Value, ".", "")},
		{"empty", ""},
		{"garbage", "abc.def"},
		{"signed by other secret", NewManager([]byte("different-secret"), false).IssueSessionCookie().Value},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest(http.MethodGet, "/", nil)
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tc.value})
			if m.VerifySessionRequest(r) {
				t.Fatalf("tampered session accepted: %q", tc.value)
			}
		})
	}
}

func TestSessionExpiry(t *testing.T) {
	m := testManager()
	// Issue a token that is already expired by pinning the clock in the past.
	past := time.Now().Add(-2 * DefaultSessionTTL)
	m.now = func() time.Time { return past }
	c := m.IssueSessionCookie()

	// Restore real clock; token should now be expired.
	m.now = time.Now
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(c)
	if m.VerifySessionRequest(r) {
		t.Fatal("expired session accepted")
	}
}

func TestCSRFRoundTrip(t *testing.T) {
	m := testManager()
	tok := m.IssueCSRFToken()
	if !m.VerifyCSRFToken(tok) {
		t.Fatal("valid CSRF token rejected")
	}
}

func TestCSRFInvalid(t *testing.T) {
	m := testManager()
	tok := m.IssueCSRFToken()

	if m.VerifyCSRFToken("") {
		t.Fatal("empty CSRF token accepted")
	}
	if m.VerifyCSRFToken(flipFirstChar(tok)) {
		t.Fatal("tampered CSRF token accepted")
	}
	// A session token must not validate as a CSRF token (domain separation).
	sess := m.IssueSessionCookie().Value
	if m.VerifyCSRFToken(sess) {
		t.Fatal("session token accepted as CSRF token")
	}
}

// flipFirstChar changes the first byte of the base64 payload to invalidate the
// signature without changing length/format.
func flipFirstChar(s string) string {
	if s == "" {
		return "x"
	}
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}
