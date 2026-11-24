package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
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
	app.userFirebase = newTestUserFirebase(t)
	t.Cleanup(func() { _ = db.Close() })
	return app
}

func newTestUserFirebase(t *testing.T) *firebaseRTDB {
	t.Helper()
	profiles := make(map[string]firebaseUserProfile)
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("auth") != "test-user-token" {
			http.Error(w, "permission denied", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		mu.Lock()
		defer mu.Unlock()
		if len(parts) == 1 && parts[0] == "users.json" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(profiles)
			return
		}
		if len(parts) < 2 || parts[0] != "users" {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 2 && strings.HasSuffix(parts[1], ".json") {
			uid := strings.TrimSuffix(parts[1], ".json")
			profile, exists := profiles[uid]
			switch r.Method {
			case http.MethodGet:
				w.Header().Set("Content-Type", "application/json")
				if !exists {
					_, _ = w.Write([]byte("null"))
					return
				}
				_ = json.NewEncoder(w).Encode(profile)
			case http.MethodPut:
				profile = firebaseUserProfile{}
				if err := json.NewDecoder(r.Body).Decode(&profile); err != nil {
					http.Error(w, "invalid profile", http.StatusBadRequest)
					return
				}
				profile.UID = uid
				profiles[uid] = profile
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(profile)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if len(parts) == 4 && parts[0] == "users" && parts[2] == "petaks" && strings.HasSuffix(parts[3], ".json") && r.Method == http.MethodPut {
			uid := parts[1]
			profile, exists := profiles[uid]
			if !exists {
				http.NotFound(w, r)
				return
			}
			var plot firebaseUserPlot
			if err := json.NewDecoder(r.Body).Decode(&plot); err != nil {
				http.Error(w, "invalid plot", http.StatusBadRequest)
				return
			}
			if profile.Petaks == nil {
				profile.Petaks = make(map[string]firebaseUserPlot)
			}
			profile.Petaks[strings.TrimSuffix(parts[3], ".json")] = plot
			profiles[uid] = profile
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(plot)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "test-user-token",
		httpClient:  server.Client(),
	}
}

func seedTestUser(t *testing.T, app *App, token string) {
	t.Helper()
	app.superAdminEmail = "farmer@example.com"
	salt := []byte("test-user-salt")
	hash := pbkdf2SHA256([]byte("test-password"), salt, passwordRounds, 32)
	if _, err := app.db.Exec(
		"INSERT INTO users (email, name, phone, password_salt, password_hash, role, firebase_uid) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"farmer@example.com", "Petani", "08123456789", salt, hash, "super_admin", "test-firebase-uid",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.Exec("INSERT INTO petaks (user_email, name) VALUES (?, ?)", "farmer@example.com", "Petak A"); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.sessions[token] = Session{Email: "farmer@example.com", ExpiresAt: time.Now().Add(time.Hour)}
	app.mu.Unlock()
	if err := app.userFirebase.putUserProfile(context.Background(), firebaseUserProfile{
		UID:    "test-firebase-uid",
		Name:   "Petani",
		Email:  "farmer@example.com",
		Phone:  "08123456789",
		Role:   "super_admin",
		Petaks: map[string]firebaseUserPlot{"Petak A": {}},
	}); err != nil {
		t.Fatal(err)
	}
}

func setTestPlotDevice(t *testing.T, app *App, name, deviceID string) {
	t.Helper()
	if err := app.userFirebase.putUserPlot(context.Background(), "test-firebase-uid", name, deviceID); err != nil {
		t.Fatal(err)
	}
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
	if err := app.userFirebase.putUserPlot(context.Background(), "test-firebase-uid", "Petak B", "firebase-device-2"); err != nil {
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
	if !strings.Contains(response.Body.String(), `href="/profile">Manajemen`) {
		t.Error("super-admin management menu is missing")
	}
	if strings.Contains(response.Body.String(), "Notifikasi") || strings.Contains(response.Body.String(), "Pengaturan") {
		t.Error("placeholder menu items should not be shown")
	}

	request = httptest.NewRequest(http.MethodGet, "/?petak=Petak+D", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /?petak=Petak+D status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestDashboardWithoutAssignedPlotDoesNotShowLocalFixture(t *testing.T) {
	app := newTestApp(t)
	const token = "unassigned-dashboard-session"
	seedTestUser(t, app, token)
	if err := app.userFirebase.putUserProfile(context.Background(), firebaseUserProfile{
		UID:    "test-firebase-uid",
		Name:   "Petani",
		Email:  "farmer@example.com",
		Role:   "super_admin",
		Petaks: map[string]firebaseUserPlot{},
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", response.Code, http.StatusOK)
	}
	for _, expected := range []string{"Menunggu petak", "Menunggu super admin menambahkan petak"} {
		if !strings.Contains(body, expected) {
			t.Errorf("unassigned dashboard missing %q", expected)
		}
	}
	for _, fixture := range []string{">75 <span>cm</span>", "85 L/mnt", "Nyalakan pompa"} {
		if strings.Contains(body, fixture) {
			t.Errorf("unassigned dashboard displayed local fixture %q", fixture)
		}
	}
}

func TestRegisterAndAddPetakPersistInFirebaseAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "icare.sqlite")
	app := newTestAppAtPath(t, dbPath)
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"localId":"firebase-farmer","idToken":"test-id-token","refreshToken":"test-refresh","expiresIn":"3600","email":"farmer@example.com"}`))
	}))
	defer authServer.Close()
	app.firebase = &firebaseRTDB{
		databaseURL: "https://firebase.test",
		authBaseURL: authServer.URL,
		apiKey:      "test-api-key",
		httpClient:  authServer.Client(),
		devicesPath: "devices",
	}
	app.superAdminEmail = "admin@example.com"

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
	profile, found, err := app.userFirebase.getUserProfile(context.Background(), "firebase-farmer")
	if err != nil {
		t.Fatal(err)
	}
	if !found || profile.Name != "Petani" || profile.Email != "farmer@example.com" || profile.Phone != "08123456789" || profile.Role != "user" {
		t.Fatalf("registered Firebase profile = %+v, found = %t", profile, found)
	}
	cookie := registerResponse.Result().Cookies()[0]
	app.superAdminEmail = "farmer@example.com"

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
	restartedApp.firebase = app.firebase
	restartedApp.userFirebase = app.userFirebase
	restartedApp.superAdminEmail = "farmer@example.com"
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
	if dashboardResponse.Code != http.StatusOK || !strings.Contains(body, "Petak B") || !strings.Contains(body, "firebase-device-2") {
		t.Fatalf("dashboard after restart did not include saved account plots: status %d", dashboardResponse.Code)
	}
}

func TestRegularUserCannotAddPlot(t *testing.T) {
	app := newTestApp(t)
	const token = "regular-user-session"
	salt := []byte("regular-salt")
	hash := pbkdf2SHA256([]byte("password123"), salt, passwordRounds, 32)
	if _, err := app.db.Exec(
		"INSERT INTO users (email, name, phone, password_salt, password_hash, role, firebase_uid) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"regular@example.com", "Pengguna", "", salt, hash, "user", "regular-firebase-uid",
	); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.sessions[token] = Session{Email: "regular@example.com", ExpiresAt: time.Now().Add(time.Hour)}
	app.mu.Unlock()

	form := url.Values{"name": {"Petak B"}, "device_id": {"ESP32_Gateway"}}
	request := httptest.NewRequest(http.MethodPost, "/plots", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("regular POST /plots status = %d, want %d", response.Code, http.StatusForbidden)
	}

	adminPlotForm := url.Values{
		"user_uid":  {"other-firebase-uid"},
		"name":      {"Petak B"},
		"device_id": {"ESP32_Gateway"},
	}
	adminPlotRequest := httptest.NewRequest(http.MethodPost, "/admin/plots", strings.NewReader(adminPlotForm.Encode()))
	adminPlotRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	adminPlotRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	adminPlotResponse := httptest.NewRecorder()
	app.ServeHTTP(adminPlotResponse, adminPlotRequest)
	if adminPlotResponse.Code != http.StatusForbidden {
		t.Fatalf("regular POST /admin/plots status = %d, want %d", adminPlotResponse.Code, http.StatusForbidden)
	}
}

func TestSuperAdminAddsFirebasePlotToUser(t *testing.T) {
	app := newTestApp(t)
	const token = "admin-add-plot-session"
	seedTestUser(t, app, token)
	target := firebaseUserProfile{
		UID:    "target-firebase-uid",
		Name:   "Pengguna",
		Email:  "target@example.com",
		Role:   "user",
		Petaks: map[string]firebaseUserPlot{},
	}
	if err := app.userFirebase.putUserProfile(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"user_uid":  {target.UID},
		"name":      {"Petak Utara"},
		"device_id": {"ESP32_Gateway"},
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/plots", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/profile" {
		t.Fatalf("admin POST /admin/plots = %d %q, want redirect to profile", response.Code, response.Header().Get("Location"))
	}
	profile, found, err := app.userFirebase.getUserProfile(context.Background(), target.UID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || profile.Petaks["Petak Utara"].DeviceID != "ESP32_Gateway" {
		t.Fatalf("target Firebase profile petaks = %+v, want Petak Utara linked to gateway", profile.Petaks)
	}
}

func TestSuperAdminDashboardShowsPlotsAssignedToUsers(t *testing.T) {
	app := newTestApp(t)
	const token = "admin-view-user-plot-session"
	seedTestUser(t, app, token)
	target := firebaseUserProfile{
		UID:    "target-firebase-uid",
		Name:   "Pengguna",
		Email:  "target@example.com",
		Role:   "user",
		Petaks: map[string]firebaseUserPlot{"Petak Timur": {DeviceID: "target-farm"}},
	}
	if err := app.userFirebase.putUserProfile(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{}

	request := httptest.NewRequest(http.MethodGet, "/?petak=Petak+Timur&owner_uid=target-firebase-uid", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET admin dashboard = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	body := response.Body.String()
	for _, expected := range []string{
		"Petak Timur · Pengguna",
		"target-farm",
		`name="owner_uid" value="target-firebase-uid"`,
		"owner_uid=target-firebase-uid",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("admin dashboard missing %q", expected)
		}
	}
}

func TestRegularUserCannotSelectAnotherUsersPlot(t *testing.T) {
	app := newTestApp(t)
	app.superAdminEmail = "admin@example.com"
	request := httptest.NewRequest(http.MethodPost, "/pump/toggle?petak=Petak+Timur&owner_uid=target-firebase-uid", nil)

	_, _, err := app.selectedDevicePlot(User{
		Email:       "user@example.com",
		FirebaseUID: "user-firebase-uid",
	}, request)
	if err != sql.ErrNoRows {
		t.Fatalf("selectedDevicePlot error = %v, want %v", err, sql.ErrNoRows)
	}
}

func TestRegularUserOnlyGetsStatisticsAndHistoryNavigation(t *testing.T) {
	app := newTestApp(t)
	const token = "regular-user-navigation-session"
	seedTestUser(t, app, token)
	app.superAdminEmail = "admin@example.com"
	app.firebase = &firebaseRTDB{}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("GET regular dashboard = %d, want %d", response.Code, http.StatusOK)
	}
	for _, expected := range []string{">Statistik</a>", ">Riwayat</a>"} {
		if !strings.Contains(body, expected) {
			t.Errorf("regular navigation missing %q", expected)
		}
	}
	for _, forbidden := range []string{`href="/devices`, `href="/profile`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("regular navigation unexpectedly contains %q", forbidden)
		}
	}

	for _, path := range []string{"/devices", "/profile"} {
		request = httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
		response = httptest.NewRecorder()
		app.ServeHTTP(response, request)
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
			t.Errorf("GET %s for regular user = %d %q, want redirect to dashboard", path, response.Code, response.Header().Get("Location"))
		}
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

func TestGoogleOAuthRoutesAreDisabledUntilFirebaseIntegration(t *testing.T) {
	app := newTestApp(t)
	for _, route := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/auth/google"},
		{method: http.MethodGet, path: "/auth/google/callback"},
	} {
		response := httptest.NewRecorder()
		app.ServeHTTP(response, httptest.NewRequest(route.method, route.path, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want %d", route.method, route.path, response.Code, http.StatusNotFound)
		}
	}
}

func TestPumpControlsRejectUnlinkedPetakInsteadOfUsingLocalSimulation(t *testing.T) {
	app := newTestApp(t)
	const token = "test-session"
	seedTestUser(t, app, token)
	if err := app.userFirebase.putUserProfile(context.Background(), firebaseUserProfile{
		UID:    "test-firebase-uid",
		Name:   "Petani",
		Email:  "farmer@example.com",
		Phone:  "08123456789",
		Role:   "super_admin",
		Petaks: map[string]firebaseUserPlot{"Petak A": {}},
	}); err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	initialMode, initialPump, initialEvents := app.state.Mode, app.state.PumpOn, len(app.state.Events)
	app.mu.Unlock()

	modeForm := url.Values{"mode": {"manual"}}
	modeRequest := httptest.NewRequest(http.MethodPost, "/pump/mode", strings.NewReader(modeForm.Encode()))
	modeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	modeRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	modeResponse := httptest.NewRecorder()
	app.ServeHTTP(modeResponse, modeRequest)
	if modeResponse.Code != http.StatusConflict {
		t.Fatalf("POST /pump/mode status = %d, want %d", modeResponse.Code, http.StatusConflict)
	}

	toggleRequest := httptest.NewRequest(http.MethodPost, "/pump/toggle", nil)
	toggleRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	toggleResponse := httptest.NewRecorder()
	app.ServeHTTP(toggleResponse, toggleRequest)
	if toggleResponse.Code != http.StatusConflict {
		t.Fatalf("POST /pump/toggle status = %d, want %d", toggleResponse.Code, http.StatusConflict)
	}

	autoForm := url.Values{"mode": {"auto"}}
	autoRequest := httptest.NewRequest(http.MethodPost, "/pump/mode", strings.NewReader(autoForm.Encode()))
	autoRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	autoRequest.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	autoResponse := httptest.NewRecorder()
	app.ServeHTTP(autoResponse, autoRequest)
	if autoResponse.Code != http.StatusConflict {
		t.Fatalf("POST /pump/mode (auto) status = %d, want %d", autoResponse.Code, http.StatusConflict)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.state.Mode != initialMode || app.state.PumpOn != initialPump || len(app.state.Events) != initialEvents {
		t.Error("an unlinked plot changed local pump simulation state")
	}
}
