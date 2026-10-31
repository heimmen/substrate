// 720-minute "stay signed in" cookie for the mf-pi agent path.
//
// The agent path (/<username>/...) authenticates with HTTP Basic on every
// request (see authenticate in main.go). Basic has no server-side lifetime: the
// browser keeps the credentials in its own cache and drops them whenever it
// feels like it — closing the browser, sleeping/locking the machine, switching
// networks. When that happens the next request arrives without Authorization,
// the auth subrequest answers 401, and the user is asked for the password again.
//
// Nothing in this server can extend the browser's cache, so we add our own
// credential: after a successful authentication the auth subrequest returns a
// signed cookie via a response header, nginx forwards it to the client
// (auth_request_set + add_header), and later requests that carry only that
// cookie are accepted. Its lifetime is ours to choose (MFPI_AUTH_COOKIE_TTL,
// default 12h = 720m).
//
// The signature is keyed by the user's stored password hash rather than a
// separate secret: nothing new to distribute, it survives restarts, and changing
// the password invalidates every outstanding cookie immediately. A cookie for one
// user cannot be replayed against another because the username is inside the
// signed payload and re-checked.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAuthCookieName = "mfpi_auth"
	defaultAuthCookieTTL  = 720 * time.Minute
	// authCookieHeader is read by nginx via auth_request_set. It is only ever
	// used on the auth subrequest, whose other headers are discarded.
	authCookieHeader = "X-Mfpi-Auth-Cookie"
)

// authCookieName is the browser-facing "stay signed in" cookie. Cookies do not
// distinguish ports, so qa/prod/test gateways on the same host would otherwise
// overwrite each other's session cookie. AUTH_COOKIE_NAME lets each deployment
// pick a distinct name (deploy.sh renders it from env/<env>.env).
var authCookieName = authCookieNameFromEnv()

func authCookieNameFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("AUTH_COOKIE_NAME")); v != "" {
		return v
	}
	return defaultAuthCookieName
}

// authCookieToken mints "user|exp|mac" for a user and their password hash.
func authCookieToken(user, passwordHash string, exp time.Time) string {
	payload := user + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(passwordHash))
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// verifyAuthCookie checks the signature and expiry for the given user.
func verifyAuthCookie(token, user, passwordHash string, now time.Time) bool {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return false
	}
	if parts[0] != user {
		return false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return false
	}
	if now.Unix() >= exp {
		return false
	}
	want := authCookieToken(parts[0], passwordHash, time.Unix(exp, 0))
	gotMAC, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	wantMAC, err := hex.DecodeString(want[strings.LastIndex(want, "|")+1:])
	if err != nil {
		return false
	}
	return hmac.Equal(gotMAC, wantMAC)
}

// authCookieState describes what a request already carries, so callers can skip
// re-issuing a cookie on every single request.
func (s *server) authCookieState(r *http.Request, user, passwordHash string) (valid bool, remaining time.Duration) {
	if s.authCookieTTL <= 0 {
		return false, 0
	}
	c, err := r.Cookie(authCookieName)
	if err != nil {
		return false, 0
	}
	if !verifyAuthCookie(c.Value, user, passwordHash, s.now()) {
		return false, 0
	}
	parts := strings.Split(c.Value, "|")
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return true, 0
	}
	// Compare against the server clock, not time.Until (wall clock): the expiry
	// was minted from s.now(), and mixing the two makes the renewal decision
	// depend on the difference between the injected clock and the real one.
	return true, time.Unix(exp, 0).Sub(s.now())
}

// setAuthCookieHeader asks nginx to hand the browser a fresh cookie. The auth
// subrequest itself cannot set cookies (its headers are dropped), so the value is
// returned as a header that nginx copies with auth_request_set + add_header.
func (s *server) setAuthCookieHeader(w http.ResponseWriter, user, passwordHash string) {
	if s.authCookieTTL <= 0 {
		return
	}
	ttl := s.authCookieTTL
	exp := s.now().Add(ttl)
	value := authCookieToken(user, passwordHash, exp)
	// Max-Age keeps it alive across browser restarts, which is the whole point.
	header := authCookieName + "=" + value +
		"; Path=/; Max-Age=" + strconv.Itoa(int(ttl.Seconds())) +
		"; HttpOnly; SameSite=Lax"
	w.Header().Set(authCookieHeader, header)
}

// refreshAuthCookie issues a cookie when the request has none, when it is more
// than half-spent (sliding renewal), or when the caller authenticated with Basic
// (which is the moment the browser is willing to accept a new one).
func (s *server) refreshAuthCookie(w http.ResponseWriter, r *http.Request, user, passwordHash string, viaBasic bool) {
	if s.authCookieTTL <= 0 {
		return
	}
	valid, remaining := s.authCookieState(r, user, passwordHash)
	if valid && !viaBasic && remaining > s.authCookieTTL/2 {
		return // fresh enough; do not churn Set-Cookie on every request
	}
	s.setAuthCookieHeader(w, user, passwordHash)
}

// authCookieTTLFromEnv parses AUTH_COOKIE_TTL ("720m", "12h", "0" to disable).
// Unset keeps the 720-minute default. The deploy-time knob is MFPI_AUTH_COOKIE_TTL;
// admin env vars carry no MFPI_ prefix (see validate-templates.sh).
func authCookieTTLFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("AUTH_COOKIE_TTL"))
	if raw == "" {
		return defaultAuthCookieTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Printf("invalid AUTH_COOKIE_TTL %q, using default %s", raw, defaultAuthCookieTTL)
		return defaultAuthCookieTTL
	}
	return d
}
