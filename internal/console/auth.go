package console

import (
	"crypto/subtle"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"dboss/internal/authcog"
	"dboss/internal/config"
)

const (
	authCallbackPath  = "/authcog"
	authStateCookie   = "app_boss_console_auth_state"
	authSessionCookie = "app_boss_console_session"
	authAudience      = "console"
	// `dboss login` mints a one-time link for a local operator; the session it creates belongs
	// to cliEmail, which AuthCog can never vouch for.
	cliLoginPath = "/login"
	cliEmail     = "cli@localhost"
	cliTokenTTL  = 3 * time.Minute
	// A hand-run session prints its console link once, in the startup banner, and the operator
	// clicks it whenever they get to it. A single-use link would be dead by then, so this one
	// stays usable for its whole life. It is only ever minted for a terminal session on the
	// loopback address, never under systemd.
	devTokenTTL = time.Hour
)

type authSession = authcog.Session

// authenticator is the console's side of the shared AuthCog flow: admins only, plus the
// one-time `dboss login` tokens.
type authenticator struct {
	flow      *authcog.Flow
	gate      authcog.Gate
	admins    map[string]bool
	mu        sync.Mutex
	cliTokens map[string]cliToken
	localPort string // console's own loopback port
}

func newAuthenticator(cfg config.Config) (*authenticator, error) {
	flow, err := authcog.New(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	return consoleAuthenticator(flow, cfg.Management, strconv.Itoa(cfg.Ports.Range[0])), nil
}

func consoleAuthenticator(flow *authcog.Flow, management config.Management, localPort string) *authenticator {
	hosts := make(map[string]bool, len(management.Host))
	for _, host := range management.Host {
		hosts[strings.ToLower(host)] = true
	}
	auth := &authenticator{flow: flow, admins: map[string]bool{}, localPort: localPort}
	for _, email := range management.Auth.AdminEmails {
		auth.admins[strings.ToLower(email)] = true
	}
	auth.gate = authcog.Gate{
		Audience:      authAudience,
		Realm:         management.Auth.Realm,
		Hosts:         func(host string) bool { return hosts[host] },
		CallbackPath:  authCallbackPath,
		StateCookie:   authStateCookie,
		SessionCookie: authSessionCookie,
		TTL:           management.Auth.SessionTTL.Value(),
		Allow:         func(email string) bool { return auth.admins[email] },
	}
	return auth
}

func (a *authenticator) authenticate(w http.ResponseWriter, r *http.Request) (authSession, bool) {
	if r.URL.Path == authCallbackPath {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return authSession{}, false
		}
		a.flow.Callback(w, r, a.gate)
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
		// from `dboss login`.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, cliLoginPage)
		return authSession{}, false
	}
	a.flow.Start(w, r, a.gate, a.loginHost(r))
	return authSession{}, false
}

const cliLoginPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Sign in to dboss</title><style>body{background:#f1f5f9;color:#182433;font:15px/1.5 Inter,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;display:grid;min-height:100vh;place-items:center;margin:0}main{max-width:32rem;padding:0 1.5rem;text-align:center}code{padding:2px 6px;border:1px solid rgb(4 32 69 / 14%);border-radius:4px;background:#fff}p{color:#667382}</style><main><h1>Sign in from the command line</h1><p>Run <code>dboss login</code> on this host and open the link it prints. The link works once and expires after 3 minutes.</p></main></html>`

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

// loginHost is the host AuthCog is asked to return to. AuthCog releases over http only to a
// local host with a port above 999, so a plain-http sign-in on port 80 goes through the
// console's own port.
func (a *authenticator) loginHost(r *http.Request) string {
	if authcog.Secure(r) || strings.Contains(r.Host, ":") {
		return r.Host
	}
	if port, _ := strconv.Atoi(a.localPort); port <= 999 {
		return r.Host
	}
	return r.Host + ":" + a.localPort
}

// cliToken is one pending login link. A `dboss login` link is spent on first use; the banner
// link of a hand-run session is not, so the same line can be clicked again while it lives.
type cliToken struct {
	expiresAt time.Time
	reusable  bool
}

// issueCLIToken returns a fresh single-use login token that expires after cliTokenTTL.
func (a *authenticator) issueCLIToken() (string, error) {
	return a.issueToken(cliTokenTTL, false)
}

// issueDevToken returns the banner link's token: longer lived and reusable, for a terminal
// session only.
func (a *authenticator) issueDevToken() (string, error) {
	return a.issueToken(devTokenTTL, true)
}

func (a *authenticator) issueToken(ttl time.Duration, reusable bool) (string, error) {
	token, err := authcog.RandomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cliTokens == nil {
		a.cliTokens = map[string]cliToken{}
	}
	for key, pending := range a.cliTokens {
		if !pending.expiresAt.After(now) {
			delete(a.cliTokens, key)
		}
	}
	a.cliTokens[token] = cliToken{expiresAt: now.Add(ttl), reusable: reusable}
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
	pending, ok := a.cliTokens[token]
	if !pending.reusable || !pending.expiresAt.After(time.Now()) {
		delete(a.cliTokens, token)
	}
	a.mu.Unlock()
	if token == "" || !ok || !pending.expiresAt.After(time.Now()) {
		http.Error(w, "login link is invalid or expired; run dboss login again", http.StatusBadRequest)
		return
	}
	if err := a.setSessionCookie(w, r, cliEmail); err != nil {
		http.Error(w, "authentication unavailable", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *authenticator) setSessionCookie(w http.ResponseWriter, r *http.Request, email string) error {
	return a.flow.SetSession(w, r, a.gate, email)
}

// validSession re-checks the admin list on every request, so removing an email ends its sessions.
func (a *authenticator) validSession(r *http.Request) (authSession, bool) {
	session, ok := a.flow.Session(r, a.gate)
	if !ok {
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
	if err != nil || origin.Scheme != authcog.Scheme(r) || !strings.EqualFold(origin.Host, r.Host) {
		return false
	}
	return true
}

func (a *authenticator) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	a.flow.ClearSession(w, r, a.gate)
}
