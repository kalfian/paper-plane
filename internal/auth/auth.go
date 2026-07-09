// Package auth provides password hashing, signed session cookies, and CSRF
// tokens for Paper Plane. Sessions and CSRF tokens are stateless: they are
// HMAC-SHA256 signed with the instance cookie secret and carry their own
// expiry, so no server-side session table is required.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kalfian/paper-plane/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// Cookie and token defaults.
const (
	// SessionCookieName is the name of the signed session cookie.
	SessionCookieName = "pp_session"

	// DefaultSessionTTL is how long a session cookie stays valid.
	DefaultSessionTTL = 7 * 24 * time.Hour
	// DefaultCSRFTTL is how long a CSRF token stays valid.
	DefaultCSRFTTL = 12 * time.Hour

	purposeSession = "session"
	purposeCSRF    = "csrf"
)

// ErrInvalidToken is returned when a signed token fails verification.
var ErrInvalidToken = errors.New("auth: invalid or expired token")

// HashPassword returns the bcrypt hash of plain at the default cost.
func HashPassword(plain string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword reports whether plain matches the bcrypt hash.
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// Manager issues and verifies signed session cookies and CSRF tokens using a
// shared HMAC secret. It is safe for concurrent use.
type Manager struct {
	secret     []byte
	secure     bool // set the Secure flag on cookies (TLS deployments)
	sessionTTL time.Duration
	csrfTTL    time.Duration
	now        func() time.Time
}

// NewManager returns a Manager signing with secret. secure controls the cookie
// Secure attribute (enable when served over HTTPS).
func NewManager(secret []byte, secure bool) *Manager {
	return &Manager{
		secret:     secret,
		secure:     secure,
		sessionTTL: DefaultSessionTTL,
		csrfTTL:    DefaultCSRFTTL,
		now:        time.Now,
	}
}

// IssueSessionCookie returns a fresh, signed session cookie.
func (m *Manager) IssueSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    m.issueToken(purposeSession, m.sessionTTL),
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  m.now().Add(m.sessionTTL),
		MaxAge:   int(m.sessionTTL / time.Second),
	}
}

// IssueSessionCookieForRequest issues a session cookie whose Secure attribute is
// set when either the manager was configured secure (APP_URL is https) OR the
// request reached us over TLS via a reverse proxy that set
// X-Forwarded-Proto: https. Paper Plane is assumed to run behind a trusted proxy
// that terminates TLS (see README), which is the only context in which
// X-Forwarded-Proto can be trusted.
func (m *Manager) IssueSessionCookieForRequest(r *http.Request) *http.Cookie {
	c := m.IssueSessionCookie()
	if !c.Secure && forwardedHTTPS(r) {
		c.Secure = true
	}
	return c
}

// forwardedHTTPS reports whether the X-Forwarded-Proto header indicates the
// original request used https. The header may be a comma-separated list added by
// chained proxies; the left-most value is the client-facing scheme.
func forwardedHTTPS(r *http.Request) bool {
	proto := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// ClearSessionCookie returns a cookie that expires the session immediately.
func (m *Manager) ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
	}
}

// VerifySessionRequest reports whether r carries a valid, unexpired session
// cookie.
func (m *Manager) VerifySessionRequest(r *http.Request) bool {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return false
	}
	return m.verifyToken(purposeSession, c.Value) == nil
}

// IssueCSRFToken returns a fresh, signed CSRF token suitable for embedding in a
// form field or request header.
func (m *Manager) IssueCSRFToken() string {
	return m.issueToken(purposeCSRF, m.csrfTTL)
}

// VerifyCSRFToken reports whether token is a valid, unexpired CSRF token.
func (m *Manager) VerifyCSRFToken(token string) bool {
	return m.verifyToken(purposeCSRF, token) == nil
}

// issueToken builds a signed token: base64url(payload) + "." + base64url(mac),
// where payload is "purpose|expUnix|nonceHex" and mac is HMAC-SHA256 over the
// raw payload string. The purpose provides domain separation between token
// kinds.
func (m *Manager) issueToken(purpose string, ttl time.Duration) string {
	exp := m.now().Add(ttl).Unix()
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		panic("auth: crypto/rand failed: " + err.Error())
	}
	payload := fmt.Sprintf("%s|%d|%s", purpose, exp, hex.EncodeToString(nonce))
	mac := m.sign([]byte(payload))

	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString(mac)
}

// verifyToken validates signature, purpose, and expiry.
func (m *Manager) verifyToken(purpose, token string) error {
	enc := base64.RawURLEncoding

	dot := strings.IndexByte(token, '.')
	if dot < 0 {
		return ErrInvalidToken
	}
	payloadB, err := enc.DecodeString(token[:dot])
	if err != nil {
		return ErrInvalidToken
	}
	macB, err := enc.DecodeString(token[dot+1:])
	if err != nil {
		return ErrInvalidToken
	}

	if !hmac.Equal(macB, m.sign(payloadB)) {
		return ErrInvalidToken
	}

	parts := strings.Split(string(payloadB), "|")
	if len(parts) != 3 || parts[0] != purpose {
		return ErrInvalidToken
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ErrInvalidToken
	}
	if m.now().Unix() >= exp {
		return ErrInvalidToken
	}
	return nil
}

// sign returns HMAC-SHA256 of msg under the manager secret.
func (m *Manager) sign(msg []byte) []byte {
	h := hmac.New(sha256.New, m.secret)
	h.Write(msg)
	return h.Sum(nil)
}

// Bootstrap ensures the instance's auth-related settings exist:
//   - admin_password_hash: bcrypt hash of adminPassword, set only if absent so
//     a later ADMIN_PASSWORD change via env does not silently reset it.
//   - cookie_secret: 32 random bytes (hex), generated once if absent.
//
// It is idempotent and safe to call on every startup.
func Bootstrap(ctx context.Context, s store.Store, adminPassword string) error {
	if _, err := s.GetSetting(ctx, store.SettingAdminPasswordHash); errors.Is(err, store.ErrNotFound) {
		hash, herr := HashPassword(adminPassword)
		if herr != nil {
			return fmt.Errorf("hash admin password: %w", herr)
		}
		if serr := s.SetSetting(ctx, store.SettingAdminPasswordHash, hash); serr != nil {
			return serr
		}
	} else if err != nil {
		return err
	}

	if _, err := s.GetSetting(ctx, store.SettingCookieSecret); errors.Is(err, store.ErrNotFound) {
		secret := make([]byte, 32)
		if _, rerr := rand.Read(secret); rerr != nil {
			return fmt.Errorf("generate cookie secret: %w", rerr)
		}
		if serr := s.SetSetting(ctx, store.SettingCookieSecret, hex.EncodeToString(secret)); serr != nil {
			return serr
		}
	} else if err != nil {
		return err
	}

	return nil
}

// CookieSecret loads and decodes the instance cookie secret produced by
// Bootstrap.
func CookieSecret(ctx context.Context, s store.Store) ([]byte, error) {
	v, err := s.GetSetting(ctx, store.SettingCookieSecret)
	if err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("decode cookie secret: %w", err)
	}
	return b, nil
}

// AdminPasswordHash loads the stored bcrypt hash of the admin password.
func AdminPasswordHash(ctx context.Context, s store.Store) (string, error) {
	return s.GetSetting(ctx, store.SettingAdminPasswordHash)
}
