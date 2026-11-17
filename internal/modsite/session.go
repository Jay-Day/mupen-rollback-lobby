package modsite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Moderator sessions. They live in memory only, so a restart signs everyone
// out; the cookie holds a random token and the server keeps only its hash.

const (
	cookieName      = "rmgk_mod"
	sessionLifetime = 12 * time.Hour
	sessionIdle     = 60 * time.Minute
	maxSessions     = 32
)

type session struct {
	hash     string // the token's hash, its key in the store
	admin    string // the moderator's name
	key      string // their admins.json entry (moderation.Manager.AdminKeyValid)
	ip       string // where they signed in; the session works only from there
	created  time.Time
	lastSeen time.Time // their last own request (the pages' refreshes don't count)
}

type sessionStore struct {
	now func() time.Time

	mu     sync.Mutex
	byHash map[string]*session
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{now: now, byHash: map[string]*session{}}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (st *sessionStore) expired(s *session, now time.Time) bool {
	return now.Sub(s.created) >= sessionLifetime || now.Sub(s.lastSeen) >= sessionIdle
}

// create starts a session and returns its token (256 random bits). Past
// maxSessions the least recently used one ends.
func (st *sessionStore) create(admin, key, ip string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	for h, s := range st.byHash {
		if st.expired(s, now) {
			delete(st.byHash, h)
		}
	}
	for len(st.byHash) >= maxSessions {
		var oldest string
		var at time.Time
		for h, s := range st.byHash {
			if oldest == "" || s.lastSeen.Before(at) {
				oldest, at = h, s.lastSeen
			}
		}
		delete(st.byHash, oldest)
	}
	h := hashToken(token)
	st.byHash[h] = &session{hash: h, admin: admin, key: key, ip: ip, created: now, lastSeen: now}
	return token, nil
}

// get returns the request's session, nil when there's none or it ended (an
// ended one is dropped). keyValid checks the moderator is still in
// admins.json with the same password; touch counts the request as activity.
func (st *sessionStore) get(r *http.Request, ip string, keyValid func(string) bool, touch bool) *session {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" || len(c.Value) > 128 {
		return nil
	}
	h := hashToken(c.Value)
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.byHash[h]
	if s == nil {
		return nil
	}
	if st.expired(s, now) || !keyValid(s.key) {
		delete(st.byHash, h)
		return nil
	}
	if s.ip != ip {
		// A stolen cookie from another address: refuse it, and end the
		// session so whoever holds it can't keep trying.
		delete(st.byHash, h)
		return nil
	}
	if touch {
		s.lastSeen = now
	}
	out := *s
	return &out
}

// end drops the request's session, if any.
func (st *sessionStore) end(r *http.Request) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return
	}
	st.mu.Lock()
	delete(st.byHash, hashToken(c.Value))
	st.mu.Unlock()
}

// othersAt counts the live sessions from ip other than the one hashed except.
func (st *sessionStore) othersAt(ip, except string) int {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for h, s := range st.byHash {
		if h != except && s.ip == ip && !st.expired(s, now) {
			n++
		}
	}
	return n
}

func (st *sessionStore) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.byHash)
}

// setCookie sets (or with "" clears) the session cookie. No Secure flag: the
// site is plain HTTP (there is no TLS certificate), reached over an SSH
// tunnel or a firewalled port; a Secure cookie would never be sent back.
// HttpOnly keeps it from scripts and SameSite=Strict from other sites.
func setCookie(w http.ResponseWriter, token string) {
	c := &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if token == "" {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}
