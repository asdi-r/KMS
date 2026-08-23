// Package auth: password hashing, JWT issuing/verification, request
// principals, and a small login rate limiter.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

type Principal struct {
	UserID   int64
	Username string
	Role     string
	Kind     string // user | apikey
}

func (p Principal) Actor() string {
	if p.Kind == "apikey" {
		return "apikey"
	}
	return "user:" + p.Username
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context { return context.WithValue(ctx, ctxKey{}, p) }
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// ---- passwords ----

func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), 12)
	return string(b), err
}

func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ---- JWT ----

type Tokens struct {
	secret []byte
	ttl    time.Duration
}

func NewTokens(secret string, ttl time.Duration) *Tokens { return &Tokens{secret: []byte(secret), ttl: ttl} }

type claims struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

func (t *Tokens) Issue(userID int64, username, role string) (string, time.Time, error) {
	exp := time.Now().Add(t.ttl)
	c := claims{Username: username, Role: role, RegisteredClaims: jwt.RegisteredClaims{
		Subject: itoa(userID), IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(exp), Issuer: "kms",
	}}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(t.secret)
	return s, exp, err
}

func (t *Tokens) Verify(token string) (Principal, error) {
	var c claims
	_, err := jwt.ParseWithClaims(token, &c, func(tk *jwt.Token) (any, error) {
		if tk.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return t.secret, nil
	}, jwt.WithIssuer("kms"), jwt.WithExpirationRequired())
	if err != nil {
		return Principal{}, err
	}
	return Principal{UserID: atoi(c.Subject), Username: c.Username, Role: c.Role, Kind: "user"}, nil
}

// ---- middleware ----

// Middleware authenticates with `Authorization: Bearer <jwt>` (users) or
// `X-API-Key` (machine access, admin role). Unauthenticated -> 401.
func Middleware(t *Tokens, apiKey string, onFail func(w http.ResponseWriter, code int, msg string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if k := r.Header.Get("X-API-Key"); k != "" {
				if apiKey != "" && subtle.ConstantTimeCompare([]byte(k), []byte(apiKey)) == 1 {
					next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{Username: "apikey", Role: RoleAdmin, Kind: "apikey"})))
					return
				}
				onFail(w, 401, "invalid api key")
				return
			}
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, "Bearer ") {
				p, err := t.Verify(strings.TrimSpace(h[7:]))
				if err != nil {
					onFail(w, 401, "invalid or expired token")
					return
				}
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
				return
			}
			onFail(w, 401, "authentication required")
		})
	}
}

// RequireRole gates a handler on a role; admin satisfies everything.
func RequireRole(role string, onFail func(w http.ResponseWriter, code int, msg string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok || (p.Role != RoleAdmin && p.Role != role) {
				onFail(w, 403, "insufficient role")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- login limiter: N failures per key (ip|username) within window -> lock ----

type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= l.max
}

func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits[key] = append(l.prune(key), time.Now())
}

func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}

func (l *Limiter) prune(key string) []time.Time {
	cut := time.Now().Add(-l.window)
	var keep []time.Time
	for _, t := range l.hits[key] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = keep
	}
	return keep
}

// ---- opaque tokens (activation tokens) ----

// NewOpaqueToken returns a random URL-safe token and its sha256 hex hash.
func NewOpaqueToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func atoi(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
