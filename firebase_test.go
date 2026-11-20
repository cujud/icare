package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestFirebaseReadsDeviceSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/devices/device-1/data_terkini.json" && r.URL.Path != "/devices/device-1/status_pompa.json" && r.URL.Path != "/devices/device-1/desired.json" && r.URL.Path != "/devices/device-1/riwayat.json") {
			t.Errorf("Firebase request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("auth") != "test-id-token" {
			t.Errorf("auth query = %q, want Firebase ID token", r.URL.Query().Get("auth"))
		}
		writeFirebaseDeviceBranch(w, r)
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
	if snapshot.Latest == nil || snapshot.Pump == nil || snapshot.Desired == nil {
		t.Fatalf("snapshot = %+v, want current telemetry, desired control and pump status branches", snapshot)
	}
	if snapshot.Latest.Fault != "OK" || snapshot.Latest.Node != "NODE_01" || snapshot.Latest.DistanceCM == nil || *snapshot.Latest.DistanceCM != 23.2 {
		t.Fatalf("latest snapshot = %+v, want exported sensor values", snapshot.Latest)
	}
	if snapshot.Pump == nil || snapshot.Pump.StatusReason != "MISSING_CONFIG" || snapshot.Pump.PumpOn == nil || *snapshot.Pump.PumpOn {
		t.Fatalf("pump snapshot = %+v, want exported device state", snapshot.Pump)
	}
	if snapshot.Desired.AutoMode == nil || !*snapshot.Desired.AutoMode || snapshot.Desired.RelayOn == nil || !*snapshot.Desired.RelayOn {
		t.Fatalf("desired snapshot = %+v, want autoMode and relayOn fields", snapshot.Desired)
	}
	history, err := client.getDeviceHistory(context.Background(), "device-1", "latest", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Records) != 1 {
		t.Fatalf("history entries = %d, want one exported record", len(history.Records))
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
		if r.URL.Path != "/devices/ESP32_Gateway/data_terkini.json" && r.URL.Path != "/devices/ESP32_Gateway/status_pompa.json" && r.URL.Path != "/devices/ESP32_Gateway/desired.json" {
			t.Errorf("Firebase path = %q", r.URL.Path)
		}
		if r.URL.Query().Has("auth") {
			t.Error("anonymous Firebase request unexpectedly included an auth token")
		}
		writeFirebaseDeviceBranch(w, r)
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
	if client.canWrite() {
		t.Fatal("anonymous read client must never be treated as Firebase write credentials")
	}
	if err := client.updateDeviceDesired(context.Background(), "ESP32_Gateway", devicePumpDesiredUpdate{
		AutoMode:    boolPointer(false),
		UpdatedAtMS: time.Now().UnixMilli(),
	}); err == nil {
		t.Fatal("anonymous Firebase client unexpectedly accepted a pump write")
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
		if r.Method != http.MethodGet || (r.URL.Path != "/devices/farm-1/data_terkini.json" && r.URL.Path != "/devices/farm-1/status_pompa.json" && r.URL.Path != "/devices/farm-1/desired.json" && r.URL.Path != "/devices/farm-1/riwayat.json") {
			t.Errorf("Firebase request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("auth") != "test-id-token" {
			t.Errorf("auth query = %q, want Firebase ID token", r.URL.Query().Get("auth"))
		}
		writeFirebaseDeviceBranch(w, r)
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
	for _, expected := range []string{"Terhubung ke Firebase", "Jarak sensor ke air", `id="distance-value">23.2</span> <span>cm</span>`, "Tinggi air sawah 76.8 cm", "OK", "Belum dikalibrasi", "MISSING_CONFIG", `name="device_id" value=""`, "Firebase Realtime Database", `id="water-chart-line"`, `id="sensor-readiness"`, `id="flow-rate-value"`, `id="device-stale-alert"`, "data-chart-start-ms=", "y1=\"108.0\"", "y1=\"136.0\"", "BERKALA_1_MENIT"} {
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
		if r.Method != http.MethodGet || (r.URL.Path != "/devices/farm-1/data_terkini.json" && r.URL.Path != "/devices/farm-1/status_pompa.json" && r.URL.Path != "/devices/farm-1/desired.json" && r.URL.Path != "/devices/farm-1/riwayat.json") {
			t.Errorf("Firebase request = %s %s", r.Method, r.URL.Path)
		}
		writeFirebaseDeviceBranch(w, r)
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
	if strings.Contains(response.Body.String(), "Lebih lama") {
		t.Error("history page showed a next page when fewer than 50 records exist")
	}
}

func TestHistoryPageRendersAtMostFiftyRowsAndCursorLinks(t *testing.T) {
	history := make(map[string]deviceEvent, 55)
	now := time.Now()
	for index := 0; index < 55; index++ {
		height := float64(index)
		history[fmt.Sprintf("key-%03d", index)] = deviceEvent{
			TimestampMS:   now.Add(time.Duration(index) * time.Minute).UnixMilli(),
			LocalTime:     fmt.Sprintf("time-%03d", index),
			WaterHeightCM: &height,
		}
	}
	firebaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/riwayat.json") {
			writeFirebaseDeviceBranch(w, r)
			return
		}
		query := r.URL.Query()
		if query.Get("orderBy") != `"$key"` {
			http.Error(w, "missing orderBy", http.StatusBadRequest)
			return
		}
		keys := make([]string, 0, len(history))
		cursor := ""
		if raw := query.Get("endAt"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &cursor)
		}
		for key := range history {
			if cursor == "" || key <= cursor {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		limit, _ := strconv.Atoi(query.Get("limitToLast"))
		if len(keys) > limit {
			keys = keys[len(keys)-limit:]
		}
		result := make(map[string]deviceEvent, len(keys))
		for _, key := range keys {
			result[key] = history[key]
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer firebaseServer.Close()

	app := newTestApp(t)
	const token = "history-pagination-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{databaseURL: firebaseServer.URL, authToken: "test-token", devicesPath: "devices", httpClient: firebaseServer.Client()}

	request := httptest.NewRequest(http.MethodGet, "/history", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if got := strings.Count(response.Body.String(), "data-event-key="); got != 50 {
		t.Fatalf("first page row count = %d, want 50", got)
	}
	if !strings.Contains(response.Body.String(), "before=key-005") {
		t.Fatal("first page did not render the older-page cursor")
	}

	request = httptest.NewRequest(http.MethodGet, "/history?petak=Petak+A&before=key-005&page=2", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if got := strings.Count(response.Body.String(), "data-event-key="); got != 5 {
		t.Fatalf("second page row count = %d, want 5", got)
	}
	if !strings.Contains(response.Body.String(), "after=key-004") || !strings.Contains(response.Body.String(), "page=1") {
		t.Fatal("second page did not render the previous-page cursor")
	}
}

func TestFirebaseHistoryUsesFiftyRecordCursorPages(t *testing.T) {
	history := make(map[string]deviceEvent, 120)
	for index := 0; index < 120; index++ {
		history[fmt.Sprintf("key-%03d", index)] = deviceEvent{TimestampMS: int64(index + 1)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devices/device-1/riwayat.json" || r.URL.Query().Get("orderBy") != `"$key"` {
			t.Errorf("Firebase history request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		query := r.URL.Query()
		cursor := ""
		if raw := query.Get("endAt"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &cursor)
		}
		if raw := query.Get("startAt"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &cursor)
		}
		keys := make([]string, 0, len(history))
		for key := range history {
			if cursor != "" && query.Has("endAt") && key > cursor {
				continue
			}
			if cursor != "" && query.Has("startAt") && key < cursor {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		limit, err := strconv.Atoi(query.Get("limitToLast"))
		if query.Has("limitToFirst") {
			limit, err = strconv.Atoi(query.Get("limitToFirst"))
		}
		if err != nil {
			t.Errorf("Firebase history limit is invalid: %v", err)
			return
		}
		if query.Has("limitToLast") && len(keys) > limit {
			keys = keys[len(keys)-limit:]
		} else if query.Has("limitToFirst") && len(keys) > limit {
			keys = keys[:limit]
		}
		result := make(map[string]deviceEvent, len(keys))
		for _, key := range keys {
			result[key] = history[key]
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()

	client := &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "token",
		devicesPath: "devices",
		httpClient:  server.Client(),
	}
	latest, err := client.getDeviceHistory(context.Background(), "device-1", "latest", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(latest.Records) != firebaseHistoryPageSize || !latest.HasOlder || latest.OldestKey != "key-070" || latest.NewestKey != "key-119" {
		t.Fatalf("latest history page = %+v, want 50 records key-070 through key-119 and older page", latest)
	}
	older, err := client.getDeviceHistory(context.Background(), "device-1", "before", latest.OldestKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Records) != firebaseHistoryPageSize || !older.HasOlder || !older.HasNewer || older.OldestKey != "key-020" || older.NewestKey != "key-069" {
		t.Fatalf("older history page = %+v, want adjacent 50 records and both navigation directions", older)
	}
	newer, err := client.getDeviceHistory(context.Background(), "device-1", "after", older.NewestKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(newer.Records) != firebaseHistoryPageSize || newer.HasNewer || !newer.HasOlder || newer.OldestKey != "key-070" || newer.NewestKey != "key-119" {
		t.Fatalf("newer history page = %+v, want the latest 50 records", newer)
	}
}

func TestDeviceWebSocketStreamsAuthenticatedFirebaseUpdates(t *testing.T) {
	firebaseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		var event string
		switch r.URL.Path {
		case "/devices/farm-1/data_terkini.json":
			event = `event: put` + "\n" + `data: {"path":"/","data":{"pompa_menyala":true,"id_node":"NODE_LIVE"}}` + "\n\n"
		case "/devices/farm-1/status_pompa.json":
			event = `event: put` + "\n" + `data: {"path":"/","data":{"mode_operasi":"MANUAL"}}` + "\n\n"
		case "/devices/farm-1/riwayat.json":
			event = `event: put` + "\n" + `data: {"path":"/","data":{"key-live":{"mode_operasi":"MANUAL","pompa_menyala":true,"waktu_epoch_ms":1795079683000,"waktu_lokal":"2026-11-19 17:14:43"}}}` + "\n\n"
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, event)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer firebaseServer.Close()

	app := newTestApp(t)
	const token = "firebase-websocket-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{
		databaseURL: firebaseServer.URL,
		authToken:   "test-id-token",
		devicesPath: "devices",
		httpClient:  firebaseServer.Client(),
	}
	appServer := httptest.NewServer(app)
	defer appServer.Close()

	header := http.Header{}
	header.Set("Cookie", sessionCookieName+"="+token)
	websocketURL := "ws" + strings.TrimPrefix(appServer.URL, "http") + "/ws/device?petak=Petak+A"
	conn, _, err := websocket.DefaultDialer.Dial(websocketURL, header)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	foundTelemetry := false
	for range 6 {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		var message deviceRealtimeMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			t.Fatal(err)
		}
		if message.Type == "telemetry" && strings.Contains(string(message.Data), `"NODE_LIVE"`) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(message.Data, &fields); err != nil {
				t.Fatalf("telemetry data is not a JSON object: %v", err)
			}
			if _, ok := fields["id_node"]; !ok {
				t.Fatal("telemetry JSON object does not contain id_node")
			}
			foundTelemetry = true
			break
		}
	}
	if !foundTelemetry {
		t.Fatal("websocket did not forward Firebase telemetry")
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
	for _, expected := range []string{"Data perangkat tidak dapat dimuat dari Firebase", `id="distance-value">—</span> <span>cm</span>`, "Firebase tidak terhubung"} {
		if !strings.Contains(body, expected) {
			t.Errorf("offline dashboard missing %q", expected)
		}
	}
	if strings.Contains(body, ">75 <span>cm</span>") {
		t.Error("dashboard displayed demo telemetry for a linked but unreachable device")
	}
}

func TestFirebasePumpToggleRequiresWriteCredentials(t *testing.T) {
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
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("pump toggle response = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(response.Body.String(), "tidak memiliki akses tulis") {
		t.Errorf("pump toggle response body = %q, want missing Firebase write credentials message", response.Body.String())
	}
}

func TestFirebasePumpToggleWritesDesiredStateAndWaitsForAck(t *testing.T) {
	var patch devicePumpDesiredUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("auth") != "test-token" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data_terkini.json"):
			_, _ = io.WriteString(w, `{"pompa_menyala":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status_pompa.json"):
			_, _ = io.WriteString(w, `{"mode_operasi":"MANUAL","pompa_menyala":false,"waktu_update_server":1795089478329}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/desired.json"):
			_, _ = io.WriteString(w, `{"autoMode":false,"relayOn":false,"updatedAtMs":1795089466821}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/desired.json"):
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode desired patch: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"autoMode":false,"relayOn":true}`)
		default:
			http.Error(w, "unexpected Firebase request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-pump-toggle-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{
		databaseURL: server.URL,
		authToken:   "test-token",
		devicesPath: "devices",
		httpClient:  server.Client(),
	}

	form := url.Values{"petak": {"Petak A"}}
	request := httptest.NewRequest(http.MethodPost, "/pump/toggle", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("pump toggle response = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	var result pumpCommandResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "pending" || result.ExpectedMode != "manual" || result.ExpectedPumpOn == nil || !*result.ExpectedPumpOn || result.UpdatedAtMS <= 0 {
		t.Fatalf("command result = %+v, want pending manual-on confirmation", result)
	}
	if patch.AutoMode == nil || *patch.AutoMode || patch.RelayOn == nil || !*patch.RelayOn || patch.UpdatedAtMS != result.UpdatedAtMS {
		t.Fatalf("Firebase desired patch = %+v, want autoMode=false relayOn=true and matching timestamp", patch)
	}
}

func TestFirebasePumpToggleRejectsAutomaticMode(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data_terkini.json"):
			_, _ = io.WriteString(w, `{"pompa_menyala":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status_pompa.json"):
			_, _ = io.WriteString(w, `{"mode_operasi":"OTOMATIS","pompa_menyala":false,"waktu_update_server":1795089478329}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/desired.json"):
			_, _ = io.WriteString(w, `{"autoMode":true,"relayOn":true,"updatedAtMs":1795089466821}`)
		case r.Method == http.MethodPatch:
			writes++
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected Firebase request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-pump-auto-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{databaseURL: server.URL, authToken: "test-token", devicesPath: "devices", httpClient: server.Client()}

	form := url.Values{"petak": {"Petak A"}}
	request := httptest.NewRequest(http.MethodPost, "/pump/toggle", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || writes != 0 {
		t.Fatalf("automatic-mode toggle response = %d, writes = %d; want conflict and no Firebase write", response.Code, writes)
	}
}

func TestFirebasePumpToggleWaitsForPreviousCommandConfirmation(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data_terkini.json"):
			_, _ = io.WriteString(w, `{"pompa_menyala":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status_pompa.json"):
			_, _ = io.WriteString(w, `{"mode_operasi":"MANUAL","pompa_menyala":false,"waktu_update_server":200}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/desired.json"):
			_, _ = io.WriteString(w, `{"autoMode":false,"relayOn":true,"updatedAtMs":201}`)
		case r.Method == http.MethodPatch:
			writes++
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected Firebase request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-pending-command-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{databaseURL: server.URL, authToken: "test-token", devicesPath: "devices", httpClient: server.Client()}

	form := url.Values{"petak": {"Petak A"}}
	request := httptest.NewRequest(http.MethodPost, "/pump/toggle", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || writes != 0 {
		t.Fatalf("pending-command response = %d, writes = %d; want conflict and no duplicate Firebase write", response.Code, writes)
	}
}

func TestFirebaseModeControlWritesDesiredAutoMode(t *testing.T) {
	var patch devicePumpDesiredUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data_terkini.json"):
			_, _ = io.WriteString(w, `{"pompa_menyala":false}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status_pompa.json"):
			_, _ = io.WriteString(w, `{"mode_operasi":"MANUAL","pompa_menyala":false,"waktu_update_server":1795089478329}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/desired.json"):
			_, _ = io.WriteString(w, `{"autoMode":false,"relayOn":false,"updatedAtMs":1795089466821}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/desired.json"):
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode desired patch: %v", err)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected Firebase request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-pump-mode-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{databaseURL: server.URL, authToken: "test-token", devicesPath: "devices", httpClient: server.Client()}

	form := url.Values{"petak": {"Petak A"}, "mode": {"auto"}}
	request := httptest.NewRequest(http.MethodPost, "/pump/mode", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("pump mode response = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	var result pumpCommandResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExpectedMode != "auto" || result.ExpectedPumpOn != nil {
		t.Fatalf("command result = %+v, want pending automatic-mode confirmation", result)
	}
	if patch.AutoMode == nil || !*patch.AutoMode || patch.RelayOn != nil || patch.UpdatedAtMS != result.UpdatedAtMS {
		t.Fatalf("Firebase desired patch = %+v, want autoMode=true without overwriting relayOn", patch)
	}
}

func TestSwitchingToManualPreservesCurrentRelayState(t *testing.T) {
	var patch devicePumpDesiredUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/data_terkini.json"):
			_, _ = io.WriteString(w, `{"pompa_menyala":true}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status_pompa.json"):
			_, _ = io.WriteString(w, `{"mode_operasi":"OTOMATIS","pompa_menyala":true,"waktu_update_server":1795089478329}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/desired.json"):
			_, _ = io.WriteString(w, `{"autoMode":true,"relayOn":false,"updatedAtMs":1795089466821}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/desired.json"):
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Errorf("decode desired patch: %v", err)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected Firebase request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-pump-manual-mode-session"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{databaseURL: server.URL, authToken: "test-token", devicesPath: "devices", httpClient: server.Client()}

	form := url.Values{"petak": {"Petak A"}, "mode": {"manual"}}
	request := httptest.NewRequest(http.MethodPost, "/pump/mode", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("manual mode response = %d, want %d: %s", response.Code, http.StatusAccepted, response.Body.String())
	}
	var result pumpCommandResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if patch.AutoMode == nil || *patch.AutoMode || patch.RelayOn == nil || !*patch.RelayOn {
		t.Fatalf("manual-mode desired patch = %+v, want manual mode and unchanged active relay", patch)
	}
	if result.ExpectedPumpOn == nil || !*result.ExpectedPumpOn {
		t.Fatalf("manual-mode result = %+v, want acknowledgment for unchanged active relay", result)
	}
}

const gatewayExportJSON = `{"data_terkini":{"data_air_terbaru":true,"debit_air_lpm":0,"faktor_kalibrasi_flow":0.2,"flow_sudah_kalibrasi":false,"id_node":"NODE_01","jam_sistem_valid":true,"jarak_sensor_ke_air_cm":23.2,"jeda_data_sensor_ms":177,"koneksi_lora":true,"modul_rtc_terpasang":true,"pompa_menyala":false,"sensor_air_valid":true,"sensor_terhubung":true,"sinyal_lora_dbm":-49,"status_gangguan":"OK","status_sensor_air":"OK","tinggi_air_sawah_cm":76.8,"total_volume_liter":0,"waktu_lokal":"2026-11-19 17:14:55","waktu_rtc_valid":true,"waktu_terima_epoch_ms":1795079694000,"waktu_update_server":1795079698126},"desired":{"autoMode":true,"relayOn":true,"updatedAtMs":1795079696000},"riwayat":{"-P6n4ZTLy3TdF0I-nnOP":{"debit_air_lpm":0,"id_node":"NODE_01","jarak_sensor_ke_air_cm":23.2,"keterangan_status":"MISSING_CONFIG","koneksi_lora":true,"mode_operasi":"MENUNGGU_KONFIGURASI","pemicu_pencatatan":"BERKALA_1_MENIT","pompa_menyala":false,"sinyal_lora_dbm":-51,"status_gangguan":"OK","status_sensor_air":"OK","tinggi_air_sawah_cm":76.8,"total_volume_liter":0,"waktu_epoch_ms":1795079683000,"waktu_lokal":"2026-11-19 17:14:43","waktu_upload_server":1795079686038}},"status_pompa":{"batas_mati_otomatis_cm":70,"batas_nyala_otomatis_cm":90,"id_sesi":"c7b994aa21061edb","keterangan_status":"MISSING_CONFIG","mode_operasi":"MENUNGGU_KONFIGURASI","pompa_menyala":false,"sensor_siap":true,"waktu_update_server":1795079698126}}`

func writeFirebaseDeviceBranch(w http.ResponseWriter, r *http.Request) {
	var snapshot deviceSnapshot
	if err := json.Unmarshal([]byte(gatewayExportJSON), &snapshot); err != nil {
		http.Error(w, "invalid test fixture", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(r.URL.Path, "/data_terkini.json"):
		_ = json.NewEncoder(w).Encode(snapshot.Latest)
	case strings.HasSuffix(r.URL.Path, "/status_pompa.json"):
		_ = json.NewEncoder(w).Encode(snapshot.Pump)
	case strings.HasSuffix(r.URL.Path, "/desired.json"):
		_ = json.NewEncoder(w).Encode(snapshot.Desired)
	case strings.HasSuffix(r.URL.Path, "/riwayat.json"):
		if r.URL.Query().Get("orderBy") != `"$key"` {
			http.Error(w, "missing history ordering", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(snapshot.History)
	default:
		http.NotFound(w, r)
	}
}

func TestDashboardEnablesRelayControlOnlyInConfirmedManualMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status_pompa.json") {
			_, _ = io.WriteString(w, `{"mode_operasi":"MANUAL","pompa_menyala":false,"waktu_update_server":1795089478329}`)
			return
		}
		writeFirebaseDeviceBranch(w, r)
	}))
	defer server.Close()

	app := newTestApp(t)
	const token = "firebase-pump-control-dashboard"
	seedTestUser(t, app, token)
	if _, err := app.db.Exec("UPDATE petaks SET device_id = ? WHERE user_email = ?", "farm-1", "farmer@example.com"); err != nil {
		t.Fatal(err)
	}
	app.firebase = &firebaseRTDB{databaseURL: server.URL, authToken: "test-token", devicesPath: "devices", httpClient: server.Client()}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard response = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(body, `data-device-control-available="true"`) || !strings.Contains(body, `data-pump-mode="manual"`) {
		t.Fatal("dashboard did not expose the confirmed Firebase control state")
	}
	if strings.Contains(body, `id="pump-toggle-button" class="button button-primary button-wide" type="submit" disabled`) {
		t.Fatal("manual-mode pump toggle should be enabled when write credentials and desired state are available")
	}
	if !strings.Contains(body, `action="/pump/mode"`) || !strings.Contains(body, `data-device-control="true"`) {
		t.Fatal("dashboard did not render Firebase-backed control forms")
	}
}

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
	if snapshot.Desired == nil || snapshot.Desired.AutoMode == nil || !*snapshot.Desired.AutoMode ||
		snapshot.Desired.RelayOn == nil || !*snapshot.Desired.RelayOn || snapshot.Desired.UpdatedAtMS != 1795079696000 {
		t.Fatalf("desired = %+v, want desired-state command fields from Firebase export", snapshot.Desired)
	}
	if snapshot.Pump == nil || snapshot.Pump.AutoOffCM == nil || *snapshot.Pump.AutoOffCM != 70 ||
		snapshot.Pump.AutoOnCM == nil || *snapshot.Pump.AutoOnCM != 90 {
		t.Fatalf("pump = %+v, want automatic threshold fields from Firebase export", snapshot.Pump)
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
		writeFirebaseDeviceBranch(w, r)
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
