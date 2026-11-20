package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGoogleOAuthCreatesLocalAccountAndSession(t *testing.T) {
	t.Setenv("ICARE_GOOGLE_CLIENT_ID", "")
	t.Setenv("ICARE_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("ICARE_GOOGLE_REDIRECT_URL", "")

	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Method != http.MethodPost || r.ParseForm() != nil {
				t.Errorf("Google token request = %s %s", r.Method, r.URL.Path)
				http.Error(w, "invalid token request", http.StatusBadRequest)
				return
			}
			if r.Form.Get("code") != "google-code" || r.Form.Get("code_verifier") == "" ||
				r.Form.Get("client_id") != "test-client" || r.Form.Get("client_secret") != "test-secret" {
				t.Errorf("Google token form omitted authorization code or PKCE credentials")
				http.Error(w, "invalid token form", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"google-access-token","token_type":"Bearer"}`))
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer google-access-token" {
				t.Errorf("Google userinfo authorization header = %q", r.Header.Get("Authorization"))
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sub":"google-subject","email":"Google.User@example.com","email_verified":true,"name":"Google User"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	app := newTestApp(t)
	app.googleOAuth = &googleOAuthConfig{
		clientID:         "test-client",
		clientSecret:     "test-secret",
		redirectURL:      "https://icare.example/auth/google/callback",
		authorizationURL: provider.URL + "/authorize",
		tokenURL:         provider.URL + "/token",
		userInfoURL:      provider.URL + "/userinfo",
		httpClient:       provider.Client(),
	}

	startForm := url.Values{"intent": {"register"}, "terms": {"on"}}
	startRequest := httptest.NewRequest(http.MethodPost, "/auth/google", strings.NewReader(startForm.Encode()))
	startRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	startResponse := httptest.NewRecorder()
	app.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusFound {
		t.Fatalf("Google auth start status = %d, want %d", startResponse.Code, http.StatusFound)
	}
	authorizationURL, err := url.Parse(startResponse.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	if query.Get("scope") != "openid email profile" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("Google authorization query = %v, want OpenID scopes and PKCE", query)
	}
	challenge := sha256.Sum256([]byte(app.googleStates[query.Get("state")].Verifier))
	if query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]) {
		t.Fatal("Google authorization URL has an invalid PKCE challenge")
	}
	var stateCookie *http.Cookie
	for _, cookie := range startResponse.Result().Cookies() {
		if cookie.Name == googleStateCookieName {
			stateCookie = cookie
			break
		}
	}
	if stateCookie == nil {
		t.Fatal("Google auth start did not set a state cookie")
	}

	callbackRequest := httptest.NewRequest(
		http.MethodGet,
		"/auth/google/callback?code=google-code&state="+url.QueryEscape(query.Get("state")),
		nil,
	)
	callbackRequest.AddCookie(stateCookie)
	callbackResponse := httptest.NewRecorder()
	app.ServeHTTP(callbackResponse, callbackRequest)
	if callbackResponse.Code != http.StatusSeeOther || callbackResponse.Header().Get("Location") != "/" {
		t.Fatalf("Google callback = %d %q, want redirect to dashboard", callbackResponse.Code, callbackResponse.Header().Get("Location"))
	}

	user, err := app.getUser("google.user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if user.Name != "Google User" || user.Phone != "" {
		t.Fatalf("Google user = %+v, want verified Google profile and empty phone", user)
	}
	var plotCount int
	if err := app.db.QueryRow("SELECT COUNT(*) FROM petaks WHERE user_email = ?", user.Email).Scan(&plotCount); err != nil {
		t.Fatal(err)
	}
	if plotCount != 1 {
		t.Fatalf("Google user plot count = %d, want one default plot", plotCount)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range callbackResponse.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("Google callback did not create an iCare session")
	}
}

func TestGetOrCreateGoogleUserRejectsUnverifiedEmail(t *testing.T) {
	t.Setenv("ICARE_GOOGLE_CLIENT_ID", "")
	t.Setenv("ICARE_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("ICARE_GOOGLE_REDIRECT_URL", "")
	app := newTestApp(t)
	_, err := app.getOrCreateGoogleUser(googleUserProfile{
		Subject: "google-subject",
		Email:   "unverified@example.com",
	})
	if err == nil {
		t.Fatal("unverified Google email unexpectedly created a local account")
	}
	if _, err := app.getUser("unverified@example.com"); err == nil {
		t.Fatal("unverified Google email was stored in the local database")
	}
}

func TestGetOrCreateGoogleUserTruncatesNameWithoutBreakingUTF8(t *testing.T) {
	t.Setenv("ICARE_GOOGLE_CLIENT_ID", "")
	t.Setenv("ICARE_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("ICARE_GOOGLE_REDIRECT_URL", "")
	app := newTestApp(t)
	user, err := app.getOrCreateGoogleUser(googleUserProfile{
		Email:         "long-name@example.com",
		EmailVerified: true,
		Name:          strings.Repeat("a", 199) + "é",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(user.Name) || len([]rune(user.Name)) != 200 {
		t.Fatalf("Google user name has invalid truncation: %q", user.Name)
	}
}

func TestGoogleOAuthConfigRequiresAllCredentials(t *testing.T) {
	t.Setenv("ICARE_GOOGLE_CLIENT_ID", "client-id")
	t.Setenv("ICARE_GOOGLE_CLIENT_SECRET", "")
	t.Setenv("ICARE_GOOGLE_REDIRECT_URL", "https://icare.example/auth/google/callback")
	if _, err := googleOAuthConfigFromEnv(); err == nil {
		t.Fatal("partial Google OAuth configuration was accepted")
	}
}
