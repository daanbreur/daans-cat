package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie  = "cat_session"
	sessionTTL     = 14 * 24 * time.Hour
	loginWindow    = 15 * time.Minute
	loginMaxPerIP  = 5
	loginMaxGlobal = 30 // a botnet spreading guesses across many IPs still hits this
)

type session struct {
	csrf    string
	expires time.Time
}

// Sessions are held in memory only. A restart logs you out, which is a fair
// trade for having no session secret on disk and instant, total revocation.
type sessionStore struct {
	mu sync.Mutex
	m  map[string]session
}

func newSessionStore() *sessionStore {
	s := &sessionStore{m: make(map[string]session)}
	go s.reap()
	return s
}

func (s *sessionStore) reap() {
	for range time.Tick(time.Hour) {
		now := time.Now()
		s.mu.Lock()
		for k, v := range s.m {
			if now.After(v.expires) {
				delete(s.m, k)
			}
		}
		s.mu.Unlock()
	}
}

func (s *sessionStore) create() (token string, csrf string) {
	token, csrf = randomToken(), randomToken()
	s.mu.Lock()
	s.m[token] = session{csrf: csrf, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return token, csrf
}

func (s *sessionStore) get(token string) (session, bool) {
	if token == "" {
		return session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[token]
	if !ok || time.Now().After(v.expires) {
		delete(s.m, token)
		return session{}, false
	}
	return v, true
}

func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("out of randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// limiter throttles login attempts per IP and in aggregate, so an attacker
// cannot grind the password even though bcrypt already makes each guess slow.
type limiter struct {
	mu     sync.Mutex
	perIP  map[string][]time.Time
	global []time.Time
}

func newLimiter() *limiter {
	return &limiter{perIP: make(map[string][]time.Time)}
}

func (l *limiter) allow(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-loginWindow)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.global = after(l.global, cutoff)
	if len(l.global) >= loginMaxGlobal {
		return false
	}
	hits := after(l.perIP[ip], cutoff)
	if len(hits) >= loginMaxPerIP {
		l.perIP[ip] = hits
		return false
	}

	l.perIP[ip] = append(hits, now)
	l.global = append(l.global, now)
	if len(l.perIP) > 10_000 { // don't let a spoofed-IP flood eat memory
		l.perIP = map[string][]time.Time{ip: l.perIP[ip]}
	}
	return true
}

// succeed clears an IP's strikes so a legitimate typo-then-success doesn't
// leave you locked out.
func (l *limiter) succeed(ip string) {
	l.mu.Lock()
	delete(l.perIP, ip)
	l.mu.Unlock()
}

func after(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func (a *App) checkPassword(pw string) bool {
	return bcrypt.CompareHashAndPassword(a.cfg.PassHash, []byte(pw)) == nil
}

// clientIP trusts X-Forwarded-For only when BEHIND_PROXY is set. Trusting it
// unconditionally would let anyone bypass the rate limiter with a header.
func (a *App) clientIP(r *http.Request) string {
	if a.cfg.BehindProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first, _, ok := strings.Cut(xff, ","); ok {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *App) setSessionCookie(w http.ResponseWriter, token string, ttl time.Duration) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cfg.SecureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl.Seconds()),
	}
	if ttl <= 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

func (a *App) currentSession(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	return a.sessions.get(c.Value)
}

func checkCSRF(s session, got string) bool {
	return subtle.ConstantTimeCompare([]byte(s.csrf), []byte(got)) == 1
}
