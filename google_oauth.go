package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	googleStateCookieName    = "icare_google_state"
	googleStateCookiePath    = "/auth/google/callback"
	googleOAuthStateDuration = 10 * time.Minute
	googleAuthorizeURL       = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL           = "https://oauth2.googleapis.com/token"
	googleUserInfoURL        = "https://openidconnect.googleapis.com/v1/userinfo"
)

type googleOAuthConfig struct {
	clientID         string
	clientSecret     string
	redirectURL      string
	authorizationURL string
	tokenURL         string
	userInfoURL      string
	httpClient       *http.Client
}

type googleOAuthState struct {
	Verifier  string
	Intent    string
	ExpiresAt time.Time
}

type googleUserProfile struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func googleOAuthConfigFromEnv() (*googleOAuthConfig, error) {
	clientID := strings.TrimSpace(os.Getenv("ICARE_GOOGLE_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("ICARE_GOOGLE_CLIENT_SECRET"))
	redirectURL := strings.TrimSpace(os.Getenv("ICARE_GOOGLE_REDIRECT_URL"))
	if clientID == "" && clientSecret == "" && redirectURL == "" {
		return nil, nil
	}
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("ICARE_GOOGLE_CLIENT_ID, ICARE_GOOGLE_CLIENT_SECRET, and ICARE_GOOGLE_REDIRECT_URL must all be configured")
	}
	parsedRedirect, err := url.Parse(redirectURL)
	if err != nil || (parsedRedirect.Scheme != "https" && parsedRedirect.Scheme != "http") ||
		parsedRedirect.Host == "" || parsedRedirect.User != nil ||
		parsedRedirect.Path != googleStateCookiePath || parsedRedirect.Fragment != "" {
		return nil, fmt.Errorf("ICARE_GOOGLE_REDIRECT_URL must be an absolute HTTP(S) URL ending in %s", googleStateCookiePath)
	}
	return &googleOAuthConfig{
		clientID:         clientID,
		clientSecret:     clientSecret,
		redirectURL:      redirectURL,
		authorizationURL: googleAuthorizeURL,
		tokenURL:         googleTokenURL,
		userInfoURL:      googleUserInfoURL,
		httpClient:       &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (a *App) startGoogleOAuth(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}
	intent := r.FormValue("intent")
	if intent != "register" && intent != "login" {
		http.Error(w, "Tujuan autentikasi Google tidak valid.", http.StatusBadRequest)
		return
	}
	if intent == "register" && r.FormValue("terms") != "on" {
		a.renderGoogleAuthError(w, intent, "Setujui syarat dan kebijakan privasi untuk melanjutkan.")
		return
	}
	if a.googleOAuth == nil {
		a.renderGoogleAuthError(w, intent, "Pendaftaran Google belum dikonfigurasi. Atur kredensial Google OAuth di server.")
		return
	}

	state, err := randomOAuthValue(32)
	if err != nil {
		http.Error(w, "Autentikasi Google tidak dapat dimulai.", http.StatusInternalServerError)
		return
	}
	verifier, err := randomOAuthValue(32)
	if err != nil {
		http.Error(w, "Autentikasi Google tidak dapat dimulai.", http.StatusInternalServerError)
		return
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	expiresAt := time.Now().Add(googleOAuthStateDuration)

	a.mu.Lock()
	for key, savedState := range a.googleStates {
		if time.Now().After(savedState.ExpiresAt) {
			delete(a.googleStates, key)
		}
	}
	a.googleStates[state] = googleOAuthState{
		Verifier:  verifier,
		Intent:    intent,
		ExpiresAt: expiresAt,
	}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     googleStateCookieName,
		Value:    state,
		Path:     googleStateCookiePath,
		Expires:  expiresAt,
		MaxAge:   int(googleOAuthStateDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	authorizationURL, err := url.Parse(a.googleOAuth.authorizationURL)
	if err != nil {
		a.renderGoogleAuthError(w, intent, "Autentikasi Google tidak dapat dimulai.")
		return
	}
	query := authorizationURL.Query()
	query.Set("client_id", a.googleOAuth.clientID)
	query.Set("redirect_uri", a.googleOAuth.redirectURL)
	query.Set("response_type", "code")
	query.Set("scope", "openid email profile")
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("prompt", "select_account")
	authorizationURL.RawQuery = query.Encode()
	http.Redirect(w, r, authorizationURL.String(), http.StatusFound)
}

func (a *App) googleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(googleStateCookieName)
	queryState := r.URL.Query().Get("state")
	if err != nil || queryState == "" || subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(queryState)) != 1 {
		a.clearGoogleStateCookie(w, r)
		a.renderGoogleAuthError(w, "login", "Sesi autentikasi Google tidak valid atau sudah kedaluwarsa. Coba lagi.")
		return
	}

	a.mu.Lock()
	state, exists := a.googleStates[queryState]
	delete(a.googleStates, queryState)
	a.mu.Unlock()
	a.clearGoogleStateCookie(w, r)
	if !exists || time.Now().After(state.ExpiresAt) {
		a.renderGoogleAuthError(w, "login", "Sesi autentikasi Google tidak valid atau sudah kedaluwarsa. Coba lagi.")
		return
	}
	if r.URL.Query().Get("error") != "" {
		a.renderGoogleAuthError(w, state.Intent, "Proses autentikasi Google dibatalkan.")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		a.renderGoogleAuthError(w, state.Intent, "Google tidak mengirim kode autentikasi. Coba lagi.")
		return
	}

	profile, err := a.fetchGoogleProfile(r.Context(), code, state.Verifier)
	if err != nil {
		a.renderGoogleAuthError(w, state.Intent, "Akun Google tidak dapat diverifikasi. Coba lagi.")
		return
	}
	user, err := a.getOrCreateGoogleUser(profile)
	if err != nil {
		a.renderGoogleAuthError(w, state.Intent, "Akun Google terverifikasi, tetapi akun iCare tidak dapat disiapkan.")
		return
	}
	if err := a.startSession(w, r, user.Email); err != nil {
		http.Error(w, "Sesi tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) fetchGoogleProfile(ctx context.Context, code, verifier string) (googleUserProfile, error) {
	var profile googleUserProfile
	form := url.Values{
		"client_id":     {a.googleOAuth.clientID},
		"client_secret": {a.googleOAuth.clientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {a.googleOAuth.redirectURL},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.googleOAuth.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return profile, errors.New("create Google token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := a.googleOAuth.httpClient.Do(request)
	if err != nil {
		return profile, errors.New("exchange Google authorization code")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return profile, errors.New("Google token exchange was rejected")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil ||
		token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return profile, errors.New("Google returned an invalid token response")
	}

	request, err = http.NewRequestWithContext(ctx, http.MethodGet, a.googleOAuth.userInfoURL, nil)
	if err != nil {
		return profile, errors.New("create Google profile request")
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	response, err = a.googleOAuth.httpClient.Do(request)
	if err != nil {
		return profile, errors.New("read Google profile")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return profile, errors.New("Google profile request was rejected")
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&profile); err != nil {
		return profile, errors.New("decode Google profile")
	}
	profile.Email = normalizeEmail(profile.Email)
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Subject == "" || !profile.EmailVerified || !validEmail(profile.Email) {
		return googleUserProfile{}, errors.New("Google profile is not verified")
	}
	if profile.Name == "" || !utf8.ValidString(profile.Name) {
		profile.Name = strings.SplitN(profile.Email, "@", 2)[0]
	}
	return profile, nil
}

func (a *App) getOrCreateGoogleUser(profile googleUserProfile) (User, error) {
	if !profile.EmailVerified || !validEmail(profile.Email) {
		return User{}, errors.New("Google email is not verified")
	}
	email := normalizeEmail(profile.Email)
	name := strings.TrimSpace(profile.Name)
	if name == "" || !utf8.ValidString(name) {
		name = strings.SplitN(email, "@", 2)[0]
	}
	if utf8.RuneCountInString(name) > 200 {
		name = string([]rune(name)[:200])
	}

	salt := make([]byte, 16)
	randomPassword := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return User{}, fmt.Errorf("create Google account password salt: %w", err)
	}
	if _, err := rand.Read(randomPassword); err != nil {
		return User{}, fmt.Errorf("create Google account password: %w", err)
	}
	passwordHash := pbkdf2SHA256(randomPassword, salt, passwordRounds, 32)

	tx, err := a.db.Begin()
	if err != nil {
		return User{}, fmt.Errorf("begin Google account transaction: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.Exec(
		"INSERT INTO users (email, name, phone, password_salt, password_hash) VALUES (?, ?, ?, ?, ?) ON CONFLICT(email) DO NOTHING",
		email, name, "", salt, passwordHash,
	)
	if err != nil {
		return User{}, fmt.Errorf("create Google user: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return User{}, fmt.Errorf("check Google user creation: %w", err)
	}
	if inserted > 0 {
		if _, err := tx.Exec("INSERT INTO petaks (user_email, name) VALUES (?, ?)", email, "Petak A"); err != nil {
			return User{}, fmt.Errorf("create default Google user plot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit Google user transaction: %w", err)
	}
	user, err := a.getUser(email)
	if err != nil {
		return User{}, fmt.Errorf("load Google user: %w", err)
	}
	return user, nil
}

func randomOAuthValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (a *App) clearGoogleStateCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     googleStateCookieName,
		Value:    "",
		Path:     googleStateCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
}

func (a *App) renderGoogleAuthError(w http.ResponseWriter, intent, message string) {
	page := "login"
	title := "Masuk"
	if intent == "register" {
		page = "register"
		title = "Buat akun"
	}
	a.render(w, page, PageData{Title: title, IsAuth: true, Error: message})
}
