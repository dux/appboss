package console

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
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"app-boss/internal/config"
)

const (
	authCallbackPath    = "/authcog"
	authStateCookie     = "app_boss_console_auth_state"
	authSessionCookie   = "app_boss_console_session"
	authStateTTL        = 5 * time.Minute
	maxAuthChallenges   = 4096
	maxAuthResponseSize = 1 << 20
	// `appboss login` mints a one-time link for a local operator; the session it creates belongs
	// to cliEmail, which AuthCog can never vouch for.
	cliLoginPath = "/login"
	cliEmail     = "cli@localhost"
	cliTokenTTL  = 3 * time.Minute
)

type authProfile struct {
	Email string `json:"email"`
}

type authSession struct {
	Email     string `json:"email"`
	CSRF      string `json:"csrf"`
	ExpiresAt int64  `json:"expires_at"`
}

type authChallenge struct {
	Destination string
	RedirectTo  string
	ExpiresAt   time.Time
}

type authenticator struct {
	cfg        config.ManagementAuth
	hosts      map[string]bool
	key        []byte
	admins     map[string]bool
	client     *http.Client
	exchange   func(context.Context, string, string) (authProfile, error)
	mu         sync.Mutex
	challenges map[string]authChallenge
	cliTokens  map[string]time.Time
}

func newAuthenticator(cfg config.Config) (*authenticator, error) {
	key, err := loadAuthKey(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	hosts := make(map[string]bool, len(cfg.Management.Host))
	for _, host := range cfg.Management.Host {
		hosts[strings.ToLower(host)] = true
	}
	auth := &authenticator{
		cfg:    cfg.Management.Auth,
		hosts:  hosts,
		key:    key,
		admins: map[string]bool{},
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		challenges: map[string]authChallenge{},
	}
	for _, email := range cfg.Management.Auth.AdminEmails {
		auth.admins[strings.ToLower(email)] = true
	}
	auth.exchange = auth.exchangeProfile
	return auth, nil
}

func (a *authenticator) authenticate(w http.ResponseWriter, r *http.Request) (authSession, bool) {
	if r.URL.Path == authCallbackPath {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return authSession{}, false
		}
		a.callback(w, r)
		return authSession{}, false
	}
	if r.URL.Path == cliLoginPath {
		a.cliLogin(w, r)
		return authSession{}, false
	}
	if session, ok := a.validSession(r); ok {
		return session, true
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"authentication required"}`+"\n")
		return authSession{}, false
	}
	if loopbackHost(r.Host) {
		// AuthCog has no destination for a loopback name, so the only way in here is the link
		// from `appboss login`.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, cliLoginPage)
		return authSession{}, false
	}
	a.startLogin(w, r)
	return authSession{}, false
}

const cliLoginPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to App Boss</title><style>body{background:#f1f5f9;color:#182433;font:15px/1.5 Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:grid;min-height:100vh;place-items:center;margin:0}main{max-width:32rem;padding:0 1.5rem;text-align:center}code{padding:2px 6px;border:1px solid rgb(4 32 69 / 14%);border-radius:4px;background:#fff}p{color:#667382}</style><main><h1>Sign in from the command line</h1><p>Run <code>appboss login</code> on this host and open the link it prints. The link works once and expires after 3 minutes.</p></main></html>`

// loopbackHost reports whether rawHost names this machine: localhost or a loopback address,
// with or without a port.
func loopbackHost(rawHost string) bool {
	host := rawHost
	if parsedHost, _, err := net.SplitHostPort(rawHost); err == nil {
		host = parsedHost
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *authenticator) startLogin(w http.ResponseWriter, r *http.Request) {
	destination, err := authDestination(r.Host, a.hosts)
	if err != nil {
		http.Error(w, "invalid authentication destination", http.StatusBadRequest)
		return
	}
	state, err := randomToken()
	if err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	redirectTo := safeRedirect(r.URL.RequestURI())
	now := time.Now()
	a.mu.Lock()
	for key, challenge := range a.challenges {
		if !challenge.ExpiresAt.After(now) {
			delete(a.challenges, key)
		}
	}
	if len(a.challenges) >= maxAuthChallenges {
		a.mu.Unlock()
		http.Error(w, "authentication is busy", http.StatusServiceUnavailable)
		return
	}
	a.challenges[state] = authChallenge{Destination: destination, RedirectTo: redirectTo, ExpiresAt: now.Add(authStateTTL)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: authStateCookie, Value: state, Path: "/", MaxAge: int(authStateTTL.Seconds()), HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	login := url.URL{Scheme: "https", Host: a.cfg.Realm, Path: destination}
	query := login.Query()
	query.Set("state", state)
	query.Set("redirect_to", redirectTo)
	login.RawQuery = query.Encode()
	http.Redirect(w, r, login.String(), http.StatusFound)
}

func (a *authenticator) callback(w http.ResponseWriter, r *http.Request) {
	state, callback := r.URL.Query().Get("state"), r.URL.Query().Get("callback")
	cookie, err := r.Cookie(authStateCookie)
	if err != nil || state == "" || callback == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		http.Error(w, "invalid authentication callback", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	challenge, ok := a.challenges[state]
	delete(a.challenges, state)
	a.mu.Unlock()
	a.clearCookie(w, r, authStateCookie)
	if !ok || !challenge.ExpiresAt.After(time.Now()) {
		http.Error(w, "expired authentication callback", http.StatusBadRequest)
		return
	}
	destination, err := authDestination(r.Host, a.hosts)
	if err != nil || destination != challenge.Destination {
		http.Error(w, "authentication destination changed", http.StatusBadRequest)
		return
	}
	profile, err := a.exchange(r.Context(), challenge.Destination, callback)
	if err != nil {
		log.Printf("management AuthCog exchange: %v", err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}
	email := strings.ToLower(strings.TrimSpace(profile.Email))
	if !a.admins[email] {
		http.Error(w, "email is not an administrator", http.StatusForbidden)
		return
	}
	if err := a.setSessionCookie(w, r, email); err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, challenge.RedirectTo, http.StatusSeeOther)
}

// issueCLIToken returns a fresh single-use login token that expires after cliTokenTTL.
func (a *authenticator) issueCLIToken() (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cliTokens == nil {
		a.cliTokens = map[string]time.Time{}
	}
	for key, expiresAt := range a.cliTokens {
		if !expiresAt.After(now) {
			delete(a.cliTokens, key)
		}
	}
	a.cliTokens[token] = now.Add(cliTokenTTL)
	return token, nil
}

func (a *authenticator) cliLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := r.URL.Query().Get("token")
	a.mu.Lock()
	expiresAt, ok := a.cliTokens[token]
	delete(a.cliTokens, token)
	a.mu.Unlock()
	if token == "" || !ok || !expiresAt.After(time.Now()) {
		http.Error(w, "login link is invalid or expired; run appboss login again", http.StatusBadRequest)
		return
	}
	if err := a.setSessionCookie(w, r, cliEmail); err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *authenticator) exchangeProfile(ctx context.Context, destination, callback string) (authProfile, error) {
	exchangeURL := url.URL{Scheme: "https", Host: a.cfg.Realm, Path: destination}
	query := exchangeURL.Query()
	query.Set("user", callback)
	exchangeURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, exchangeURL.String(), nil)
	if err != nil {
		return authProfile{}, err
	}
	response, err := a.client.Do(request)
	if err != nil {
		return authProfile{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAuthResponseSize))
		return authProfile{}, fmt.Errorf("AuthCog returned %s", response.Status)
	}
	var profile authProfile
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAuthResponseSize)).Decode(&profile); err != nil {
		return authProfile{}, err
	}
	if profile.Email == "" {
		return authProfile{}, errors.New("AuthCog response has no email")
	}
	return profile, nil
}

func (a *authenticator) setSessionCookie(w http.ResponseWriter, r *http.Request, email string) error {
	csrf, err := randomToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(a.cfg.SessionTTL.Value())
	payload, err := json.Marshal(authSession{Email: email, CSRF: csrf, ExpiresAt: expires.Unix()})
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := a.sign(encoded)
	// Lax, not Strict: the callback redirects to the console inside a navigation that AuthCog
	// started cross-site, and browsers withhold Strict cookies on that whole redirect chain, so
	// Strict would bounce every fresh login straight back to the login page. Mutations are still
	// protected by the CSRF token and the Origin check.
	http.SetCookie(w, &http.Cookie{Name: authSessionCookie, Value: encoded + "." + signature, Path: "/", Expires: expires, MaxAge: int(a.cfg.SessionTTL.Value().Seconds()), HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	return nil
}

func (a *authenticator) validSession(r *http.Request) (authSession, bool) {
	cookie, err := r.Cookie(authSessionCookie)
	if err != nil {
		return authSession{}, false
	}
	encoded, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return authSession{}, false
	}
	expected, err := base64.RawURLEncoding.DecodeString(a.sign(encoded))
	if err != nil {
		return authSession{}, false
	}
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(actual, expected) {
		return authSession{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return authSession{}, false
	}
	var session authSession
	if json.Unmarshal(payload, &session) != nil || session.CSRF == "" || session.ExpiresAt <= time.Now().Unix() {
		return authSession{}, false
	}
	if email := strings.ToLower(session.Email); !a.admins[email] && email != cliEmail {
		return authSession{}, false
	}
	return session, true
}

func (a *authenticator) validCSRF(r *http.Request, session authSession) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(session.CSRF)) != 1 {
		return false
	}
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Scheme != requestScheme(r) || !strings.EqualFold(origin.Host, r.Host) {
		return false
	}
	return true
}

func (a *authenticator) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, r, authSessionCookie)
}

func (a *authenticator) clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
}

func (a *authenticator) sign(value string) string {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// authDestination turns the request host into the AuthCog destination path, or fails when the
// host is not one of the console's configured hostnames.
func authDestination(rawHost string, hosts map[string]bool) (string, error) {
	host, port := rawHost, ""
	if parsedHost, parsedPort, err := net.SplitHostPort(rawHost); err == nil {
		host, port = parsedHost, parsedPort
	} else if strings.Contains(rawHost, ":") {
		return "", errors.New("invalid host port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !hosts[host] {
		return "", errors.New("unexpected host")
	}
	destination := "/d:" + host
	if port != "" {
		if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
			return "", errors.New("invalid port")
		}
		destination += "/p:" + port
	}
	return destination, nil
}

func safeRedirect(target string) string {
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.Contains(target, "\\") || strings.HasPrefix(target, authCallbackPath) {
		return "/"
	}
	return target
}

func requestScheme(r *http.Request) string {
	if secureRequest(r) {
		return "https"
	}
	return "http"
}

func secureRequest(r *http.Request) bool {
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https") {
		return true
	}
	host, port := r.Host, ""
	if parsedHost, parsedPort, err := net.SplitHostPort(r.Host); err == nil {
		host, port = parsedHost, parsedPort
	}
	local := strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".lvh.me") || net.ParseIP(host) != nil
	if local {
		value, _ := strconv.Atoi(port)
		return value <= 999
	}
	return true
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func loadAuthKey(stateDir string) ([]byte, error) {
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "management-auth.key")
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
		return loadAuthKey(stateDir)
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
