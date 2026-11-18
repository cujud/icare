package main

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPBKDF2SHA256(t *testing.T) {
	got := hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32))
	const want = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("pbkdf2SHA256() = %s, want %s", got, want)
	}
}

func TestUnauthenticatedDashboardRedirectsToLogin(t *testing.T) {
	app, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("GET / status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if got := response.Header().Get("Location"); got != "/login" {
		t.Fatalf("GET / redirect = %q, want /login", got)
	}
}

func TestAuthenticationPagesArePublic(t *testing.T) {
	app, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/login", "/register"} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, response.Code, http.StatusOK)
		}
	}
}

func TestPumpModeAndToggleUpdateState(t *testing.T) {
	app, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	const token = "test-session"
	app.mu.Lock()
	app.users["farmer@example.com"] = User{Name: "Petani", Email: "farmer@example.com"}
	app.sessions[token] = Session{Email: "farmer@example.com", ExpiresAt: time.Now().Add(time.Hour)}
	app.mu.Unlock()

	modeForm := url.Values{"mode": {"manual"}}
	modeRequest := httptest.NewRequest(http.MethodPost, "/pump/mode", strings.NewReader(modeForm.Encode()))
	modeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	modeRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	modeResponse := httptest.NewRecorder()
	app.ServeHTTP(modeResponse, modeRequest)
	if modeResponse.Code != http.StatusSeeOther {
		t.Fatalf("POST /pump/mode status = %d, want %d", modeResponse.Code, http.StatusSeeOther)
	}

	app.mu.Lock()
	if app.state.Mode != "manual" {
		t.Errorf("pump mode = %q, want manual", app.state.Mode)
	}
	initialPumpState := app.state.PumpOn
	app.mu.Unlock()

	toggleRequest := httptest.NewRequest(http.MethodPost, "/pump/toggle", nil)
	toggleRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	toggleResponse := httptest.NewRecorder()
	app.ServeHTTP(toggleResponse, toggleRequest)
	if toggleResponse.Code != http.StatusSeeOther {
		t.Fatalf("POST /pump/toggle status = %d, want %d", toggleResponse.Code, http.StatusSeeOther)
	}

	app.mu.Lock()
	if app.state.PumpOn == initialPumpState {
		t.Error("pump state did not change after toggle")
	}
	if len(app.state.Events) != 5 {
		t.Errorf("activity count = %d, want 5", len(app.state.Events))
	}
	app.mu.Unlock()

	autoForm := url.Values{"mode": {"auto"}}
	autoRequest := httptest.NewRequest(http.MethodPost, "/pump/mode", strings.NewReader(autoForm.Encode()))
	autoRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	autoRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	autoResponse := httptest.NewRecorder()
	app.ServeHTTP(autoResponse, autoRequest)
	if autoResponse.Code != http.StatusSeeOther {
		t.Fatalf("POST /pump/mode (auto) status = %d, want %d", autoResponse.Code, http.StatusSeeOther)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if !app.state.PumpOn {
		t.Error("automatic mode should turn the pump on below the 80 cm threshold")
	}
	if len(app.state.Events) != 7 {
		t.Errorf("activity count after automatic control = %d, want 7", len(app.state.Events))
	}
}
