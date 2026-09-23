// Package authcog is the AuthCog sign-in flow shared by the management console and the proxy's
// per-app gate: the challenge, the callback exchange and the signed session cookie. It knows
// nothing about who may sign in; every caller describes itself with a Gate.
package authcog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dboss/internal/logx"
)

const (
	stateTTL        = 5 * time.Minute
	maxChallenges   = 4096
	maxResponseSize = 1 << 20
	keyFile         = "management-auth.key"
)

// Profile is what AuthCog answers for a verified callback. dboss forwards it to an app as the
// JSON X-Dboss-User header, so the JSON tags are the wire contract.
type Profile struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Avatar   string `json:"avatar"`
	Provider string `json:"provider"`
}

// Session is the signed cookie payload. Audience names the gate it was issued for, so a console
// cookie never opens an app and an app cookie never opens another app.
type Session struct {
	Email     string `json:"email"`
	CSRF      string `json:"csrf"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"expires_at"`
}

// Gate is one protected surface: the console, or one app behind the proxy. Realm is the AuthCog
// realm host (e.g. auth.authcog.com) this gate signs in against.
type Gate struct {
	Audience      string
	Realm         string
	Hosts         func(host string) bool // lowercase hostname without port
	CallbackPath  string
	StateCookie   string
	SessionCookie string
	TTL           time.Duration
	Allow         func(email string) bool // lowercase email
}

type challenge struct {
	audience    string
	destination string
	redirectTo  string
	expiresAt   time.Time
}

// Flow holds the signing key and the pending sign-ins. Exchange is the AuthCog round trip and
// is a field so tests can replace it.
type Flow struct {
	Exchange func(ctx context.Context, realm, destination, callback string) (Profile, error)

	key        []byte
	client     *http.Client
	mu         sync.Mutex
	challenges map[string]challenge
}

// New loads, or creates on first use, the signing key under stateDir.
func New(stateDir string) (*Flow, error) {
	key, err := loadKey(stateDir)
	if err != nil {
		return nil, err
	}
	return NewWithKey(key), nil
}

// NewWithKey builds a flow around a given 32-byte key.
func NewWithKey(key []byte) *Flow {
	flow := &Flow{
		key: key,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		challenges: map[string]challenge{},
	}
	flow.Exchange = flow.exchangeProfile
	return flow
}

// Start redirects the browser to AuthCog, which sends it back to the scheme, host and port the
// request arrived on.
func (f *Flow) Start(w http.ResponseWriter, r *http.Request, gate Gate) {
	destination, err := Destination(r.Host, Scheme(r), gate.Hosts)
	if err != nil {
		http.Error(w, "invalid authentication destination", http.StatusBadRequest)
		return
	}
	state, err := RandomToken()
	if err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	redirectTo := safeRedirect(r.URL.RequestURI(), gate.CallbackPath)
	now := time.Now()
	f.mu.Lock()
	for key, pending := range f.challenges {
		if !pending.expiresAt.After(now) {
			delete(f.challenges, key)
		}
	}
	if len(f.challenges) >= maxChallenges {
		f.mu.Unlock()
		http.Error(w, "authentication is busy", http.StatusServiceUnavailable)
		return
	}
	f.challenges[state] = challenge{audience: gate.Audience, destination: destination, redirectTo: redirectTo, expiresAt: now.Add(stateTTL)}
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: gate.StateCookie, Value: state, Path: "/", MaxAge: int(stateTTL.Seconds()), HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	login := url.URL{Scheme: "https", Host: gate.Realm, Path: destination}
	query := login.Query()
	query.Set("state", state)
	query.Set("redirect_to", redirectTo)
	login.RawQuery = query.Encode()
	http.Redirect(w, r, login.String(), http.StatusFound)
}

// Callback finishes a sign-in: it verifies the state, exchanges the callback for the profile,
// asks the gate whether the email may enter and sets the session cookie.
func (f *Flow) Callback(w http.ResponseWriter, r *http.Request, gate Gate) {
	pending, profile, ok := f.verify(w, r, gate)
	if !ok {
		return
	}
	if err := f.SetSession(w, r, gate, profile.Email); err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, pending.redirectTo, http.StatusSeeOther)
}

// Authenticate finishes a sign-in without issuing a session: it verifies the state, exchanges
// the callback for the profile and asks the gate whether the email may enter. It is the proxy's
// path, where the app owns the session and dboss only forwards the identity once.
func (f *Flow) Authenticate(w http.ResponseWriter, r *http.Request, gate Gate) (Profile, bool) {
	_, profile, ok := f.verify(w, r, gate)
	return profile, ok
}

// verify consumes the pending challenge and exchanges the callback for an allowed profile. It
// writes the error response itself and returns ok=false when the callback is not valid.
func (f *Flow) verify(w http.ResponseWriter, r *http.Request, gate Gate) (challenge, Profile, bool) {
	state, callback := r.URL.Query().Get("state"), r.URL.Query().Get("callback")
	cookie, err := r.Cookie(gate.StateCookie)
	if err != nil || state == "" || callback == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		http.Error(w, "invalid authentication callback", http.StatusBadRequest)
		return challenge{}, Profile{}, false
	}
	f.mu.Lock()
	pending, ok := f.challenges[state]
	delete(f.challenges, state)
	f.mu.Unlock()
	clearCookie(w, r, gate.StateCookie)
	if !ok || !pending.expiresAt.After(time.Now()) {
		http.Error(w, "expired authentication callback", http.StatusBadRequest)
		return challenge{}, Profile{}, false
	}
	destination, err := Destination(r.Host, Scheme(r), gate.Hosts)
	if err != nil || destination != pending.destination || pending.audience != gate.Audience {
		http.Error(w, "authentication destination changed", http.StatusBadRequest)
		return challenge{}, Profile{}, false
	}
	profile, err := f.Exchange(r.Context(), gate.Realm, pending.destination, callback)
	if err != nil {
		logx.Errorf("AuthCog exchange for %s: %v", gate.Audience, err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return challenge{}, Profile{}, false
	}
	profile.Email = strings.ToLower(strings.TrimSpace(profile.Email))
	if !gate.Allow(profile.Email) {
		http.Error(w, fmt.Sprintf("User with %s is not permitted to login to dboss.", profile.Email), http.StatusForbidden)
		return challenge{}, Profile{}, false
	}
	return pending, profile, true
}

func (f *Flow) exchangeProfile(ctx context.Context, realm, destination, callback string) (Profile, error) {
	exchangeURL := url.URL{Scheme: "https", Host: realm, Path: destination}
	query := exchangeURL.Query()
	query.Set("user", callback)
	exchangeURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, exchangeURL.String(), nil)
	if err != nil {
		return Profile{}, err
	}
	response, err := f.client.Do(request)
	if err != nil {
		return Profile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseSize))
		return Profile{}, fmt.Errorf("AuthCog returned %s", response.Status)
	}
	var profile Profile
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseSize)).Decode(&profile); err != nil {
		return Profile{}, err
	}
	if profile.Email == "" {
		return Profile{}, errors.New("AuthCog response has no email")
	}
	return profile, nil
}

// SetSession signs a session for email and sets the gate's cookie.
func (f *Flow) SetSession(w http.ResponseWriter, r *http.Request, gate Gate, email string) error {
	csrf, err := RandomToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(gate.TTL)
	payload, err := json.Marshal(Session{Email: email, CSRF: csrf, Audience: gate.Audience, ExpiresAt: expires.Unix()})
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	// Lax, not Strict: the callback redirects inside a navigation that AuthCog started
	// cross-site, and browsers withhold Strict cookies on that whole redirect chain, so Strict
	// would bounce every fresh login straight back to the login page.
	http.SetCookie(w, &http.Cookie{Name: gate.SessionCookie, Value: encoded + "." + f.sign(encoded), Path: "/", Expires: expires, MaxAge: int(gate.TTL.Seconds()), HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	return nil
}

// Session returns the request's session when it is signed by this flow, unexpired and issued for
// this gate. Whether the email may still enter is the caller's check, on every request.
func (f *Flow) Session(r *http.Request, gate Gate) (Session, bool) {
	cookie, err := r.Cookie(gate.SessionCookie)
	if err != nil {
		return Session{}, false
	}
	encoded, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return Session{}, false
	}
	expected, err := base64.RawURLEncoding.DecodeString(f.sign(encoded))
	if err != nil {
		return Session{}, false
	}
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(actual, expected) {
		return Session{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Session{}, false
	}
	var session Session
	if json.Unmarshal(payload, &session) != nil || session.CSRF == "" || session.ExpiresAt <= time.Now().Unix() || session.Audience != gate.Audience {
		return Session{}, false
	}
	return session, true
}

// ClearSession signs the browser out of the gate.
func (f *Flow) ClearSession(w http.ResponseWriter, r *http.Request, gate Gate) {
	clearCookie(w, r, gate.SessionCookie)
}

func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
}

func (f *Flow) sign(value string) string {
	mac := hmac.New(sha256.New, f.key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Destination is the AuthCog path that brings the browser back to where it started:
// /d:<host>, then /p:<port> when the port is not the scheme's default, then /s:http when the
// request was not https. It fails when the gate does not answer for the host.
func Destination(rawHost, scheme string, hosts func(string) bool) (string, error) {
	host, port := rawHost, ""
	if parsedHost, parsedPort, err := net.SplitHostPort(rawHost); err == nil {
		host, port = parsedHost, parsedPort
	} else if strings.Contains(rawHost, ":") {
		return "", errors.New("invalid host port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	// The host becomes a URL path segment, so nothing but hostname characters gets through.
	if host == "" || strings.Trim(host, "abcdefghijklmnopqrstuvwxyz0123456789.-") != "" || !hosts(host) {
		return "", errors.New("unexpected host")
	}
	destination := "/d:" + host
	if port != "" {
		if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
			return "", errors.New("invalid port")
		}
		if port != defaultPorts[scheme] {
			destination += "/p:" + port
		}
	}
	if scheme != "https" {
		destination += "/s:" + scheme
	}
	return destination, nil
}

var defaultPorts = map[string]string{"http": "80", "https": "443"}

func safeRedirect(target, callbackPath string) string {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, "\\") || strings.HasPrefix(target, callbackPath) {
		return "/"
	}
	return target
}

// Scheme is the scheme the browser used: https over TLS, else the first X-Forwarded-Proto an
// edge such as Cloudflare set, else http. The AuthCog return address, the Secure cookie flag and
// the console's Origin check all follow it.
func Scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])); proto == "https" || proto == "http" {
		return proto
	}
	return "http"
}

func secure(r *http.Request) bool { return Scheme(r) == "https" }

func RandomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func loadKey(stateDir string) ([]byte, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, keyFile)
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%s must contain exactly 32 bytes", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadKey(stateDir)
	}
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return key, nil
}
