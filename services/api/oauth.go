package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Browser sign-in for the OAuth cloud backends (Google Drive, Microsoft
// OneDrive/SharePoint). rclone cannot finish these from stored credentials: the
// account owner has to approve access, and the callback lands on whichever host
// the provider redirects to. So Onyx runs the authorization-code flow itself,
// then hands rclone the token it obtained — the same token rclone would have
// stored had `rclone authorize` been run on this machine, and the same token it
// refreshes on its own afterwards using the remote's client id and secret.
//
// A redirect URI has to be registered with the provider's OAuth application, so
// the console tells the operator exactly which one to register (it is returned
// by the start endpoint and printed on the callback page).

// oauthBackend is the provider's endpoint set plus the scopes rclone needs.
type oauthBackend struct {
	Type       string
	AuthURL    string
	TokenURL   string
	Scopes     []string
	AuthParams map[string]string
}

// oauthBackends covers the providers whose consent screen Onyx drives itself.
// The rest (Dropbox, Box, pCloud) stay on the paste-a-token path, which needs no
// registered application at all.
func oauthBackends() map[string]oauthBackend {
	return map[string]oauthBackend{
		"drive": {
			Type:       "drive",
			AuthURL:    "https://accounts.google.com/o/oauth2/auth",
			TokenURL:   "https://oauth2.googleapis.com/token",
			Scopes:     []string{"https://www.googleapis.com/auth/drive"},
			AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		},
		"onedrive": {
			Type:       "onedrive",
			AuthURL:    "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			TokenURL:   "https://login.microsoftonline.com/common/oauth2/v2.0/token",
			Scopes:     []string{"offline_access", "Files.ReadWrite.All", "User.Read"},
			AuthParams: map[string]string{"prompt": "consent"},
		},
		"sharepoint": {
			Type:       "sharepoint",
			AuthURL:    "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			TokenURL:   "https://login.microsoftonline.com/common/oauth2/v2.0/token",
			Scopes:     []string{"offline_access", "Files.ReadWrite.All", "Sites.ReadWrite.All"},
			AuthParams: map[string]string{"prompt": "consent"},
		},
	}
}

func oauthBackendFor(name string) (oauthBackend, bool) {
	b, ok := oauthBackends()[strings.ToLower(strings.TrimSpace(name))]
	return b, ok
}

// oauthEnvCredentials lets a deployment register one OAuth application for the
// whole platform instead of editing each remote: <TYPE>_OAUTH_CLIENT_ID /
// <TYPE>_OAUTH_CLIENT_SECRET (GOOGLE_ / MICROSOFT_ are accepted too, because
// that is what the provider consoles call them).
func oauthEnvCredentials(backendType string) (string, string) {
	prefixes := map[string][]string{
		"drive":      {"DRIVE", "GOOGLE"},
		"onedrive":   {"ONEDRIVE", "MICROSOFT"},
		"sharepoint": {"SHAREPOINT", "MICROSOFT"},
	}
	for _, prefix := range prefixes[backendType] {
		id := strings.TrimSpace(os.Getenv(prefix + "_OAUTH_CLIENT_ID"))
		secret := strings.TrimSpace(os.Getenv(prefix + "_OAUTH_CLIENT_SECRET"))
		if id != "" && secret != "" {
			return id, secret
		}
	}
	return "", ""
}

// callbackPath is where the provider sends the browser back. It is a fixed path
// because it has to be registered in the provider's console verbatim.
const callbackPath = "/api/v1/storage/oauth/callback"

// oauthCallbackURL is the redirect URI the provider must be told about. It is
// whatever the operator reached Onyx on unless the deployment pins it with
// ONYX_PUBLIC_URL or ONYX_OAUTH_REDIRECT_URI — behind a proxy the request's own
// idea of its host is the proxy's internal address, which would never match.
func oauthCallbackURL(r *http.Request) string {
	if pinned := strings.TrimRight(strings.TrimSpace(os.Getenv("ONYX_OAUTH_REDIRECT_URI")), "/"); pinned != "" {
		return pinned
	}
	if base := strings.TrimRight(strings.TrimSpace(os.Getenv("ONYX_PUBLIC_URL")), "/"); base != "" {
		return base + callbackPath
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = strings.Split(forwarded, ",")[0]
	}
	return scheme + "://" + strings.TrimSpace(host) + callbackPath
}

// pendingOAuth is one in-flight sign-in. The state is the only thing the
// provider echoes back, so it is what binds the callback to a remote.
type pendingOAuth struct {
	remote       string
	backendType  string
	redirectURI  string
	clientID     string
	clientSecret string
	created      time.Time
}

type oauthStates struct {
	mu     sync.Mutex
	states map[string]pendingOAuth
}

var pendingLogins = &oauthStates{states: map[string]pendingOAuth{}}

const oauthStateTTL = 15 * time.Minute

func (o *oauthStates) put(entry pendingOAuth) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	state := hex.EncodeToString(buf)
	entry.created = time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	// Drop anything the operator abandoned, so the map cannot grow forever.
	for key, value := range o.states {
		if time.Since(value.created) > oauthStateTTL {
			delete(o.states, key)
		}
	}
	o.states[state] = entry
	return state, nil
}

// take returns the entry and removes it: a state is single-use, so a replayed
// callback cannot store a second token.
func (o *oauthStates) take(state string) (pendingOAuth, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, ok := o.states[state]
	if !ok {
		return pendingOAuth{}, false
	}
	delete(o.states, state)
	if time.Since(entry.created) > oauthStateTTL {
		return pendingOAuth{}, false
	}
	return entry, true
}

// remoteOptions reads one remote's stored options back out of rclone, which is
// where the OAuth client id/secret live once the target has been saved.
func remoteOptions(ctx context.Context, name string) (map[string]string, error) {
	dump, err := runRclone(ctx, "config", "dump")
	if err != nil {
		return nil, err
	}
	var parsed map[string]map[string]any
	if err := json.Unmarshal([]byte(dump), &parsed); err != nil {
		return nil, fmt.Errorf("decode rclone config: %w", err)
	}
	section, ok := parsed[name]
	if !ok {
		return nil, fmt.Errorf("remote %q is not configured", name)
	}
	out := map[string]string{}
	for key, value := range section {
		if text, ok := value.(string); ok {
			out[key] = text
		}
	}
	return out, nil
}

// oauthCredentials resolves the client id/secret for a sign-in: the remote's own
// options win, the deployment-wide environment is the fallback.
func oauthCredentials(ctx context.Context, name, backendType string) (string, string, error) {
	opts, err := remoteOptions(ctx, name)
	if err != nil {
		return "", "", err
	}
	id, secret := opts["client_id"], opts["client_secret"]
	if id == "" || secret == "" {
		envID, envSecret := oauthEnvCredentials(backendType)
		if envID == "" || envSecret == "" {
			return "", "", fmt.Errorf("%s needs an OAuth application: set client_id and client_secret on the target (or %s_OAUTH_CLIENT_ID / _OAUTH_CLIENT_SECRET) and register the redirect URI with the provider", backendType, strings.ToUpper(backendType))
		}
		id, secret = envID, envSecret
	}
	return id, secret, nil
}

type oauthStartBody struct {
	// RedirectURI is echoed back rather than taken from the request when the
	// operator has already registered a different one.
	RedirectURI string `json:"redirect_uri"`
}

// handleOAuthStart serves POST /api/v1/storage/remotes/{name}/oauth/start and
// answers with the provider URL to send the operator's browser to.
func (s *server) handleOAuthStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := validateRemoteName(name); err != nil {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	opts, err := remoteOptions(ctx, name)
	if err != nil {
		writeEnvelope(w, http.StatusNotFound, apiError{Code: "not_found", Message: err.Error()})
		return
	}
	backendType := opts["type"]
	backend, ok := oauthBackendFor(backendType)
	if !ok {
		writeEnvelope(w, http.StatusBadRequest, apiError{Code: "invalid_argument", Message: fmt.Sprintf("%s is not signed in through the console: paste a token from `rclone authorize %s` instead", backendType, backendType)})
		return
	}
	var body oauthStartBody
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // an empty body is fine
	}
	clientID, clientSecret, err := oauthCredentials(ctx, name, backendType)
	if err != nil {
		writeEnvelope(w, http.StatusFailedDependency, apiError{Code: "failed_precondition", Message: err.Error()})
		return
	}
	redirectURI := strings.TrimSpace(body.RedirectURI)
	if redirectURI == "" {
		redirectURI = oauthCallbackURL(r)
	}
	state, err := pendingLogins.put(pendingOAuth{remote: name, backendType: backendType, redirectURI: redirectURI, clientID: clientID, clientSecret: clientSecret})
	if err != nil {
		writeEnvelope(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
		return
	}
	query := url.Values{}
	query.Set("client_id", clientID)
	query.Set("response_type", "code")
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", strings.Join(backend.Scopes, " "))
	query.Set("state", state)
	for key, value := range backend.AuthParams {
		query.Set(key, value)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         name,
		"type":         backendType,
		"auth_url":     backend.AuthURL + "?" + query.Encode(),
		"redirect_uri": redirectURI,
		"scopes":       backend.Scopes,
		"hint":         "Register the redirect URI above in the " + backendType + " OAuth application, then approve access; this console completes the sign-in automatically.",
	})
}

type oauthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// exchangeOAuthCodeFunc is exchangeOAuthCode behind a variable so the callback
// can be driven end to end in tests without a provider on the other end.
var exchangeOAuthCodeFunc = exchangeOAuthCode

// exchangeOAuthCode trades the authorization code for tokens.
func exchangeOAuthCode(ctx context.Context, backend oauthBackend, clientID, clientSecret, code, redirectURI string) (oauthTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return oauthTokenResponse{}, err
	}
	defer response.Body.Close()
	var parsed oauthTokenResponse
	if err := json.NewDecoder(response.Body).Decode(&parsed); err != nil {
		return oauthTokenResponse{}, fmt.Errorf("the provider's token response was not JSON: %w", err)
	}
	if parsed.Error != "" {
		message := parsed.Description
		if message == "" {
			message = parsed.Error
		}
		return oauthTokenResponse{}, fmt.Errorf("%s", message)
	}
	if parsed.AccessToken == "" {
		return oauthTokenResponse{}, fmt.Errorf("the provider returned no access token")
	}
	if parsed.TokenType == "" {
		parsed.TokenType = "Bearer"
	}
	return parsed, nil
}

// rcloneTokenJSON renders the token the way rclone stores it in a remote — the
// same shape, and the same option name, that `rclone authorize` prints.
func rcloneTokenJSON(tokens oauthTokenResponse) (string, error) {
	// rclone sends token_type verbatim, and a provider that omits it still means
	// a bearer token — an empty type would produce "Authorization: " headers.
	tokenType := tokens.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}
	token := map[string]any{
		"access_token": tokens.AccessToken,
		"token_type":   tokenType,
	}
	// A provider that returns no refresh token is a provider whose access token
	// simply lives out its lifetime; keeping the key absent (rather than empty)
	// is what rclone expects.
	if tokens.RefreshToken != "" {
		token["refresh_token"] = tokens.RefreshToken
	}
	if tokens.ExpiresIn > 0 {
		token["expiry"] = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	encoded, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// handleOAuthCallback serves GET /api/v1/storage/oauth/callback: the browser
// lands here from the provider, so it answers with a page rather than JSON.
func (s *server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	providerError := r.URL.Query().Get("error")
	if providerError != "" {
		description := r.URL.Query().Get("error_description")
		writeOAuthPage(w, http.StatusBadRequest, false, "Sign-in was refused: "+strings.TrimSpace(providerError+" "+description))
		return
	}
	entry, ok := pendingLogins.take(state)
	if !ok {
		writeOAuthPage(w, http.StatusBadRequest, false, "This sign-in link has expired or was already used. Press Connect again.")
		return
	}
	if code == "" {
		writeOAuthPage(w, http.StatusBadRequest, false, "The provider returned no authorization code.")
		return
	}
	backend, ok := oauthBackendFor(entry.backendType)
	if !ok {
		writeOAuthPage(w, http.StatusBadRequest, false, "Unknown provider "+entry.backendType+".")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	tokens, err := exchangeOAuthCodeFunc(ctx, backend, entry.clientID, entry.clientSecret, code, entry.redirectURI)
	if err != nil {
		writeOAuthPage(w, http.StatusBadGateway, false, "The provider rejected the sign-in: "+err.Error())
		return
	}
	tokenJSON, err := rcloneTokenJSON(tokens)
	if err != nil {
		writeOAuthPage(w, http.StatusInternalServerError, false, err.Error())
		return
	}
	updateCtx, updateCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer updateCancel()
	if output, err := runRclone(updateCtx, append([]string{"config", "update", entry.remote, "token", tokenJSON, "--non-interactive"}, networkBounds...)...); err != nil {
		writeOAuthPage(w, http.StatusBadGateway, false, "rclone could not store the token: "+output)
		return
	}
	probeCtx, probeCancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer probeCancel()
	output, probeErr := runRclone(probeCtx, append([]string{"lsd", entry.remote + ":", "--max-depth", "1"}, networkBounds...)...)
	if probeErr != nil {
		writeOAuthPage(w, http.StatusOK, false, "Signed in, but "+entry.remote+" could not be reached: "+output)
		return
	}
	writeOAuthPage(w, http.StatusOK, true, entry.remote+" is connected. You can close this window and return to Onyx.")
}

// writeOAuthPage answers a browser with something readable: this endpoint is
// reached by a person, not by a client.
func writeOAuthPage(w http.ResponseWriter, status int, ok bool, message string) {
	tone := "var(--onyx-danger, #d64545)"
	title := "Sign-in failed"
	if ok {
		tone = "var(--onyx-success, #2f9e44)"
		title = "Account connected"
	}
	body := "<!doctype html><meta charset=\"utf-8\"><title>" + html.EscapeString(title) + "</title>" +
		"<body style=\"font:15px/1.5 system-ui,sans-serif;background:#101014;color:#e8e8ea;display:grid;place-items:center;height:100vh;margin:0\">" +
		"<div style=\"max-width:34rem;padding:2rem;border:1px solid #2a2a30;border-radius:12px;background:#16161b\">" +
		"<h1 style=\"font-size:1.05rem;margin:0 0 .75rem;color:" + tone + "\">" + html.EscapeString(title) + "</h1>" +
		"<p style=\"margin:0 0 1rem;white-space:pre-wrap\">" + html.EscapeString(message) + "</p>" +
		"<p style=\"margin:0\"><a href=\"/#/shares\" style=\"color:#7aa2f7\">Return to the Onyx console</a></p></div></body>"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// oauthProviderTypes returns the providers the console can sign in for, so the
// UI can offer the browser flow only where it exists.
func oauthRedirectTypes() []string {
	types := make([]string, 0, 3)
	for name := range oauthBackends() {
		types = append(types, name)
	}
	sort.Strings(types)
	return types
}
