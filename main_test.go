package main

import (
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	return newTestAppAtPath(t, ":memory:")
}

func newTestAppAtPath(t *testing.T, path string) *App {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := initializeDatabase(db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	app, err := newAppWithDB(db)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return app
}

func seedTestUser(t *testing.T, app *App, token string) {
	t.Helper()
	salt := []byte("test-user-salt")
	hash := pbkdf2SHA256([]byte("test-password"), salt, passwordRounds, 32)
	if _, err := app.db.Exec(
		"INSERT INTO users (email, name, phone, password_salt, password_hash) VALUES (?, ?, ?, ?, ?)",
		"farmer@example.com", "Petani", "08123456789", salt, hash,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec("INSERT INTO petaks (user_email, name) VALUES (?, ?)", "farmer@example.com", "Petak A"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.sessions[token] = Session{Email: "farmer@example.com", ExpiresAt: time.Now().Add(time.Hour)}
	app.mu.Unlock()
}

func TestPBKDF2SHA256(t *testing.T) {
	got := hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32))
	const want = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("pbkdf2SHA256() = %s, want %s", got, want)
	}
}

func TestUnauthenticatedDashboardRedirectsToLogin(t *testing.T) {
	app := newTestApp(t)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusSeeOther {
		t.Fatalf("GET / status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if got := response.Header().Get("Location"); got != "/login" {
		t.Fatalf("GET / redirect = %q, want /login", got)
	}
}

func TestDashboardPetakSelection(t *testing.T) {
	app := newTestApp(t)
	const token = "petak-test-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec(
		"INSERT INTO petaks (user_email, name, device_id) VALUES (?, ?, ?)",
		"farmer@example.com", "Petak B", "firebase-device-2",
	); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/?petak=Petak+B", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /?petak=Petak+B status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "Sawah · Petak B") || !strings.Contains(response.Body.String(), "aria-current=\"page\">Petak B") || !strings.Contains(response.Body.String(), "firebase-device-2") {
		t.Error("dashboard did not render Petak B as the selected petak")
	}
	if !strings.Contains(response.Body.String(), "Notifikasi") || !strings.Contains(response.Body.String(), "Pengaturan") {
		t.Error("sidebar placeholder tabs are missing")
	}

	request = httptest.NewRequest(http.MethodGet, "/?petak=Petak+D", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /?petak=Petak+D status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestRegisterAndAddPetakPersistInSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "icare.sqlite")
	app := newTestAppAtPath(t, dbPath)

	registerForm := url.Values{
		"name":                  {"Petani"},
		"email":                 {"farmer@example.com"},
		"phone":                 {"08123456789"},
		"password":              {"password123"},
		"password_confirmation": {"password123"},
		"terms":                 {"on"},
	}
	registerRequest := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(registerForm.Encode()))
	registerRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	registerResponse := httptest.NewRecorder()
	app.ServeHTTP(registerResponse, registerRequest)
	if registerResponse.Code != http.StatusSeeOther {
		t.Fatalf("POST /register status = %d, want %d: %s", registerResponse.Code, http.StatusSeeOther, registerResponse.Body.String())
	}
	cookie := registerResponse.Result().Cookies()[0]

	plotForm := url.Values{"name": {"Petak B"}, "device_id": {"firebase-device-2"}}
	plotRequest := httptest.NewRequest(http.MethodPost, "/plots", strings.NewReader(plotForm.Encode()))
	plotRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	plotRequest.AddCookie(cookie)
	plotResponse := httptest.NewRecorder()
	app.ServeHTTP(plotResponse, plotRequest)
	if plotResponse.Code != http.StatusSeeOther || plotResponse.Header().Get("Location") != "/?petak=Petak+B" {
		t.Fatalf("POST /plots response = %d, %q; want redirect to new petak", plotResponse.Code, plotResponse.Header().Get("Location"))
	}

	duplicateRequest := httptest.NewRequest(http.MethodPost, "/plots", strings.NewReader(plotForm.Encode()))
	duplicateRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	duplicateRequest.AddCookie(cookie)
	duplicateResponse := httptest.NewRecorder()
	app.ServeHTTP(duplicateResponse, duplicateRequest)
	if duplicateResponse.Code != http.StatusConflict {
		t.Fatalf("duplicate POST /plots status = %d, want %d", duplicateResponse.Code, http.StatusConflict)
	}

	if err := app.db.Close(); err != nil {
		t.Fatal(err)
	}
	restartedApp := newTestAppAtPath(t, dbPath)
	loginForm := url.Values{"email": {"farmer@example.com"}, "password": {"password123"}}
	loginRequest := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(loginForm.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResponse := httptest.NewRecorder()
	restartedApp.ServeHTTP(loginResponse, loginRequest)
	if loginResponse.Code != http.StatusSeeOther {
		t.Fatalf("POST /login after restart status = %d, want %d", loginResponse.Code, http.StatusSeeOther)
	}

	dashboardRequest := httptest.NewRequest(http.MethodGet, "/?petak=Petak+B", nil)
	dashboardRequest.AddCookie(loginResponse.Result().Cookies()[0])
	dashboardResponse := httptest.NewRecorder()
	restartedApp.ServeHTTP(dashboardResponse, dashboardRequest)
	body := dashboardResponse.Body.String()
	if dashboardResponse.Code != http.StatusOK || !strings.Contains(body, "Petak A") || !strings.Contains(body, "Petak B") || !strings.Contains(body, "firebase-device-2") {
		t.Fatalf("dashboard after restart did not include saved account plots: status %d", dashboardResponse.Code)
	}
}

func TestAuthenticationPagesArePublic(t *testing.T) {
	app := newTestApp(t)
	for _, path := range []string{"/login", "/register"} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, response.Code, http.StatusOK)
		}
	}
}

func TestPumpModeAndToggleUpdateState(t *testing.T) {
	app := newTestApp(t)
	const token = "test-session"
	seedTestUser(t, app, token)

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
