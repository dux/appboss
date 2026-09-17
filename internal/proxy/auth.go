package proxy

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

	"deploy-boss/internal/config"
)

const (
	authCallbackPath  = "/authcog"
	authStateCookie   = "deploy_boss_auth_state"
	authSessionCookie = "deploy_boss_session"
	authStateTTL      = 5 * time.Minute
	maxAuthChallenges = 4096
)

type authProfile struct {
	Email string `json:"email"`
}

type authSession struct {
	Email     string `json:"email"`
	ExpiresAt int64  `json:"expires_at"`
}

type authChallenge struct {
	Destination string
	RedirectTo  string
	ExpiresAt   time.Time
}

type authenticator struct {
	cfg        config.Auth
	key        []byte
	admins     map[string]bool
	client     *http.Client
	exchange   func(context.Context, string, string) (authProfile, error)
	mu         sync.Mutex
	challenges map[string]authChallenge
}

func newAuthenticator(cfg config.Config) (*authenticator, error) {
	if len(cfg.Auth.AdminEmails) == 0 {
		return nil, nil
	}
	key, err := loadAuthKey(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	auth := &authenticator{
		cfg:    cfg.Auth,
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
	for _, email := range cfg.Auth.AdminEmails {
		auth.admins[strings.ToLower(email)] = true
	}
	auth.exchange = auth.exchangeProfile
	return auth, nil
}

func (a *authenticator) authorize(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == authCallbackPath {
		a.callback(w, r)
		return false
	}
	if a.validSession(r) {
		return true
	}
	a.startLogin(w, r)
	return false
}

func (a *authenticator) startLogin(w http.ResponseWriter, r *http.Request) {
	destination, err := authDestination(r.Host)
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
	a.clearStateCookie(w, r)
	if !ok || !challenge.ExpiresAt.After(time.Now()) {
		http.Error(w, "expired authentication callback", http.StatusBadRequest)
		return
	}
	destination, err := authDestination(r.Host)
	if err != nil || destination != challenge.Destination {
		http.Error(w, "authentication destination changed", http.StatusBadRequest)
		return
	}
	profile, err := a.exchange(r.Context(), challenge.Destination, callback)
	if err != nil {
		log.Printf("authcog exchange: %v", err)
		http.Error(w, "authentication failed", http.StatusUnauthorized)
		return
	}
	email := strings.ToLower(profile.Email)
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
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return authProfile{}, fmt.Errorf("AuthCog returned %s", response.Status)
	}
	var profile authProfile
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&profile); err != nil {
		return authProfile{}, err
	}
	if profile.Email == "" {
		return authProfile{}, errors.New("AuthCog response has no email")
	}
	return profile, nil
}

func (a *authenticator) setSessionCookie(w http.ResponseWriter, r *http.Request, email string) error {
	expires := time.Now().Add(a.cfg.SessionTTL.Value())
	payload, err := json.Marshal(authSession{Email: email, ExpiresAt: expires.Unix()})
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := a.sign(encoded)
	http.SetCookie(w, &http.Cookie{Name: authSessionCookie, Value: encoded + "." + signature, Path: "/", Expires: expires, MaxAge: int(a.cfg.SessionTTL.Value().Seconds()), HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
	return nil
}

func (a *authenticator) validSession(r *http.Request) bool {
	cookie, err := r.Cookie(authSessionCookie)
	if err != nil {
		return false
	}
	encoded, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(a.sign(encoded))
	if err != nil {
		return false
	}
	actual, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(actual, expected) {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	var session authSession
	if json.Unmarshal(payload, &session) != nil || session.ExpiresAt <= time.Now().Unix() {
		return false
	}
	return a.admins[strings.ToLower(session.Email)]
}

func (a *authenticator) sign(value string) string {
	mac := hmac.New(sha256.New, a.key)
	_, _ = mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *authenticator) clearStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: authStateCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureRequest(r), SameSite: http.SameSiteLaxMode})
}

func authDestination(rawHost string) (string, error) {
	host, port := rawHost, ""
	if parsedHost, parsedPort, err := net.SplitHostPort(rawHost); err == nil {
		host, port = parsedHost, parsedPort
	} else if strings.Contains(rawHost, ":") {
		return "", errors.New("invalid host port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || strings.ContainsAny(host, "/\\") {
		return "", errors.New("invalid host")
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
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") || strings.HasPrefix(target, authCallbackPath) {
		return "/"
	}
	return target
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func loadAuthKey(stateDir string) ([]byte, error) {
	path := filepath.Join(stateDir, "auth.key")
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
