package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFirebaseReadsDeviceSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/devices/device-1.json" {
			t.Errorf("Firebase request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("auth") != "test-id-token" {
			t.Errorf("auth query = %q, want Firebase ID token", r.URL.Query().Get("auth"))
		}
		_, _ = io.WriteString(w, gatewayExportJSON)
	}))
	defer server.Close()

	client := &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "test-id-token",
		devicesPath: "devices",
		httpClient:  server.Client(),
	}
	snapshot, err := client.getDevice(context.Background(), "device-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Latest == nil || snapshot.Pump == nil {
		t.Fatalf("snapshot = %+v, want current telemetry and pump status branches", snapshot)
	}
	if snapshot.Latest.Fault != "OK" || snapshot.Latest.Node != "NODE_01" || snapshot.Latest.DistanceCM == nil || *snapshot.Latest.DistanceCM != 23.2 {
		t.Fatalf("latest snapshot = %+v, want exported sensor values", snapshot.Latest)
	}
	if snapshot.Pump == nil || snapshot.Pump.StatusReason != "MISSING_CONFIG" || snapshot.Pump.PumpOn == nil || *snapshot.Pump.PumpOn {
		t.Fatalf("pump snapshot = %+v, want exported device state", snapshot.Pump)
	}
	if len(snapshot.History) != 1 {
		t.Fatalf("history entries = %d, want one exported record", len(snapshot.History))
	}
}

func TestFirebaseReadRequiresCredentialsForAuthRestrictedRules(t *testing.T) {
	client := &firebaseRTDB{}
	if client.ready() {
		t.Fatal("Firebase client without a token was marked ready")
	}
	if _, err := client.getDevice(context.Background(), "ESP32_Gateway"); err == nil {
		t.Fatal("getDevice succeeded without credentials")
	}
}

func TestFirebaseCanReadWithoutAuthWhenExplicitlyEnabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devices/ESP32_Gateway.json" {
			t.Errorf("Firebase path = %q", r.URL.Path)
		}
		if r.URL.Query().Has("auth") {
			t.Error("anonymous Firebase request unexpectedly included an auth token")
		}
		_, _ = io.WriteString(w, gatewayExportJSON)
	}))
	defer server.Close()

	client := &firebaseRTDB{
		databaseURL:   server.URL,
		authToken:     "ignored-token",
		anonymousRead: true,
		devicesPath:   "devices",
		httpClient:    server.Client(),
	}
	snapshot, err := client.getDevice(context.Background(), "ESP32_Gateway")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Latest == nil || snapshot.Latest.Fault != "OK" {
		t.Fatalf("snapshot = %+v, want anonymous latest telemetry", snapshot)
	}
}

func TestFirebaseAnonymousReadCanBeConfiguredFromEnvironment(t *testing.T) {
	t.Setenv("ICARE_FIREBASE_DATABASE_URL", "https://example.test")
	t.Setenv("ICARE_FIREBASE_ANONYMOUS_READ", "true")
	t.Setenv("ICARE_FIREBASE_AUTH_TOKEN", "")
	t.Setenv("ICARE_FIREBASE_REFRESH_TOKEN", "")
	t.Setenv("ICARE_FIREBASE_API_KEY", "")

	client, err := newFirebaseRTDBFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !client.ready() || !client.anonymousRead {
		t.Fatal("Firebase client did not enable anonymous reads from the environment")
	}
}

func TestFirebaseRejectsInvalidDeviceKeys(t *testing.T) {
	client := &firebaseRTDB{authToken: "test-id-token", httpClient: http.DefaultClient}
	if _, err := client.getDevice(context.Background(), "../other"); err == nil {
		t.Fatal("getDevice accepted a path-like device ID")
	}
}

func TestDashboardUsesFirebaseDeviceSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/devices/farm-1.json" {
			t.Errorf("Firebase request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("auth") != "test-id-token" {
			t.Errorf("auth query = %q, want Firebase ID token", r.URL.Query().Get("auth"))
		}
		_, _ = io.WriteString(w, gatewayExportJSON)
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-dashboard-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "test-id-token",
		devicesPath: "devices",
		httpClient:  server.Client(),
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{"Terhubung ke Firebase", "Jarak sensor ke air", ">23.2 <span>cm</span>", "Tinggi air sawah 76.8 cm", "OK", "Belum dikalibrasi", "MISSING_CONFIG", `name="device_id" value=""`, "Firebase Realtime Database"} {
		if !strings.Contains(body, expected) {
			t.Errorf("dashboard missing %q", expected)
		}
	}
	if strings.Contains(body, "75 cm") || strings.Contains(body, "Data sensor contoh") {
		t.Error("dashboard displayed demo telemetry despite a successful Firebase read")
	}
}

func TestHistoryUsesFirebaseExportRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/devices/farm-1.json" {
			t.Errorf("Firebase request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, gatewayExportJSON)
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-history-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "test-id-token",
		devicesPath: "devices",
		httpClient:  server.Client(),
	}

	request := httptest.NewRequest(http.MethodGet, "/history", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	for _, expected := range []string{"Riwayat Firebase", "MISSING_CONFIG", "Tinggi air 76.8 cm", "BERKALA_1_MENIT"} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("Firebase history page missing %q", expected)
		}
	}
}

func TestDashboardDoesNotShowDemoValuesForUnreachableDevice(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Permission denied", http.StatusUnauthorized)
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-offline-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "offline-device", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "test-id-token",
		devicesPath: "devices",
		httpClient:  server.Client(),
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard response = %d, want %d", response.Code, http.StatusOK)
	}
	for _, expected := range []string{"Data perangkat tidak dapat dimuat dari Firebase", ">— <span>cm</span>", "Firebase tidak terhubung"} {
		if !strings.Contains(body, expected) {
			t.Errorf("offline dashboard missing %q", expected)
		}
	}
	if strings.Contains(body, ">75 <span>cm</span>") {
		t.Error("dashboard displayed demo telemetry for a linked but unreachable device")
	}
}

func TestFirebasePumpToggleDisabledWithoutCommandSchema(t *testing.T) {
	app := newTestApp(t)
	const token = "firebase-pump-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec(
		"INSERT INTO petaks (user_email, name, device_id) VALUES (?, ?, ?)",
		"farmer@example.com", "Petak B", "farm-b",
	); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"petak": {"Petak B"}}
	request := httptest.NewRequest(http.MethodPost, "/pump/toggle", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("pump toggle response = %d, want %d", response.Code, http.StatusNotImplemented)
	}
	if !strings.Contains(response.Body.String(), "Skema perintah pompa belum tersedia") {
		t.Errorf("pump toggle response body = %q, want missing command schema message", response.Body.String())
	}
}

const gatewayExportJSON = `{"data_terkini":{"data_air_terbaru":true,"debit_air_lpm":0,"faktor_kalibrasi_flow":0.2,"flow_sudah_kalibrasi":false,"id_node":"NODE_01","jam_sistem_valid":true,"jarak_sensor_ke_air_cm":23.2,"jeda_data_sensor_ms":177,"koneksi_lora":true,"modul_rtc_terpasang":true,"pompa_menyala":false,"sensor_air_valid":true,"sensor_terhubung":true,"sinyal_lora_dbm":-49,"status_gangguan":"OK","status_sensor_air":"OK","tinggi_air_sawah_cm":76.8,"total_volume_liter":0,"waktu_lokal":"2026-11-19 17:14:55","waktu_rtc_valid":true,"waktu_terima_epoch_ms":1795079694000,"waktu_update_server":1795079698126},"riwayat":{"-P6n4ZTLy3TdF0I-nnOP":{"debit_air_lpm":0,"id_node":"NODE_01","jarak_sensor_ke_air_cm":23.2,"keterangan_status":"MISSING_CONFIG","koneksi_lora":true,"mode_operasi":"MENUNGGU_KONFIGURASI","pemicu_pencatatan":"BERKALA_1_MENIT","pompa_menyala":false,"sinyal_lora_dbm":-51,"status_gangguan":"OK","status_sensor_air":"OK","tinggi_air_sawah_cm":76.8,"total_volume_liter":0,"waktu_epoch_ms":1795079683000,"waktu_lokal":"2026-11-19 17:14:43","waktu_upload_server":1795079686038}},"status_pompa":{"id_sesi":"c7b994aa21061edb","keterangan_status":"MISSING_CONFIG","konfigurasi_aktif":false,"mode_operasi":"MENUNGGU_KONFIGURASI","pompa_menyala":false,"revisi_konfigurasi":0,"sensor_siap":true,"waktu_update_server":1795079698126}}`

func TestFirebaseDecodesRealGatewayExport(t *testing.T) {
	var snapshot deviceSnapshot
	if err := json.Unmarshal([]byte(gatewayExportJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	latest := snapshot.Latest
	if latest == nil || latest.DistanceCM == nil || *latest.DistanceCM != 23.2 || latest.UploadedAtMS != 1795079698126 ||
		latest.RSSI == nil || *latest.RSSI != -49 || latest.DebitLPM == nil || latest.TotalLiter == nil ||
		latest.WaterHeightCM == nil || *latest.WaterHeightCM != 76.8 {
		t.Fatalf("latest = %+v, want values from the real gateway export", latest)
	}
}

type signInRewriteTransport struct{ target string }

func (r signInRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Host, "identitytoolkit.googleapis.com") {
		forwarded, err := http.NewRequest(req.Method, r.target+req.URL.RequestURI(), req.Body)
		if err != nil {
			return nil, err
		}
		forwarded.Header = req.Header
		return http.DefaultTransport.RoundTrip(forwarded)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestFirebaseSignsInWithEmailPasswordAndCachesToken(t *testing.T) {
	signIns := 0
	identity := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		signIns++
		_, _ = io.WriteString(w, `{"idToken":"signed-in-token","refreshToken":"refresh-1","expiresIn":"3600","localId":"uid-1"}`)
	}))
	defer identity.Close()
	database := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("auth") != "signed-in-token" {
			http.Error(w, `{"error":"Permission denied"}`, http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, gatewayExportJSON)
	}))
	defer database.Close()

	client := &firebaseRTDB{
		databaseURL: database.URL,
		apiKey:      "test-key",
		email:       "gateway@example.com",
		password:    "secret",
		devicesPath: "devices",
		httpClient:  &http.Client{Transport: signInRewriteTransport{target: identity.URL}},
	}
	if !client.ready() {
		t.Fatal("client with API key, email and password must be ready")
	}
	for range 2 {
		snapshot, err := client.getDevice(context.Background(), "ESP32_Gateway")
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Latest.Node != "NODE_01" {
			t.Fatalf("node = %q, want NODE_01", snapshot.Latest.Node)
		}
	}
	if signIns != 1 {
		t.Fatalf("sign-in calls = %d, want token to be cached after the first", signIns)
	}
}
