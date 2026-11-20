package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const defaultFirebaseDatabaseURL = "https://icareiot-default-rtdb.asia-southeast1.firebasedatabase.app"
const defaultFirebaseDevicesPath = "devices"

type firebaseRTDB struct {
	databaseURL   string
	authToken     string
	apiKey        string
	refreshToken  string
	email         string
	password      string
	anonymousRead bool
	devicesPath   string
	httpClient    *http.Client
	tokenMu       sync.Mutex
	tokenExpiry   time.Time
}

type deviceSnapshot struct {
	Latest  *deviceLatest          `json:"data_terkini"`
	Desired *devicePumpDesired     `json:"desired"`
	History map[string]deviceEvent `json:"riwayat"`
	Pump    *devicePumpStatus      `json:"status_pompa"`
}

type deviceLatest struct {
	DataFresh       bool     `json:"data_air_terbaru"`
	DebitLPM        *float64 `json:"debit_air_lpm"`
	CalibrationK    *float64 `json:"faktor_kalibrasi_flow"`
	FlowCalibrated  bool     `json:"flow_sudah_kalibrasi"`
	Node            string   `json:"id_node"`
	SystemTimeValid bool     `json:"jam_sistem_valid"`
	DistanceCM      *float64 `json:"jarak_sensor_ke_air_cm"`
	SensorAgeMS     int64    `json:"jeda_data_sensor_ms"`
	LoRaOnline      bool     `json:"koneksi_lora"`
	RTCInstalled    bool     `json:"modul_rtc_terpasang"`
	PumpOn          *bool    `json:"pompa_menyala"`
	SensorValid     bool     `json:"sensor_air_valid"`
	SensorConnected bool     `json:"sensor_terhubung"`
	RSSI            *int     `json:"sinyal_lora_dbm"`
	Fault           string   `json:"status_gangguan"`
	SensorStatus    string   `json:"status_sensor_air"`
	WaterHeightCM   *float64 `json:"tinggi_air_sawah_cm"`
	TotalLiter      *float64 `json:"total_volume_liter"`
	LocalTime       string   `json:"waktu_lokal"`
	RTCValid        bool     `json:"waktu_rtc_valid"`
	ReceivedAtMS    int64    `json:"waktu_terima_epoch_ms"`
	UploadedAtMS    int64    `json:"waktu_update_server"`
}

type deviceEvent struct {
	DebitLPM      *float64 `json:"debit_air_lpm"`
	Node          string   `json:"id_node"`
	DistanceCM    *float64 `json:"jarak_sensor_ke_air_cm"`
	StatusReason  string   `json:"keterangan_status"`
	LoRaOnline    bool     `json:"koneksi_lora"`
	Mode          string   `json:"mode_operasi"`
	PumpOn        *bool    `json:"pompa_menyala"`
	RSSI          *int     `json:"sinyal_lora_dbm"`
	Fault         string   `json:"status_gangguan"`
	SensorStatus  string   `json:"status_sensor_air"`
	WaterHeightCM *float64 `json:"tinggi_air_sawah_cm"`
	TotalLiter    *float64 `json:"total_volume_liter"`
	TimestampMS   int64    `json:"waktu_epoch_ms"`
	LocalTime     string   `json:"waktu_lokal"`
	UploadedAtMS  int64    `json:"waktu_upload_server"`
	Trigger       string   `json:"pemicu_pencatatan"`
}

type devicePumpStatus struct {
	SessionID      string   `json:"id_sesi"`
	StatusReason   string   `json:"keterangan_status"`
	ConfigActive   bool     `json:"konfigurasi_aktif"`
	Mode           string   `json:"mode_operasi"`
	PumpOn         *bool    `json:"pompa_menyala"`
	ConfigRevision int      `json:"revisi_konfigurasi"`
	SensorReady    bool     `json:"sensor_siap"`
	UpdatedAtMS    int64    `json:"waktu_update_server"`
	AutoOffCM      *float64 `json:"batas_mati_otomatis_cm"`
	AutoOnCM       *float64 `json:"batas_nyala_otomatis_cm"`
}

type devicePumpDesired struct {
	AutoMode    *bool `json:"autoMode"`
	RelayOn     *bool `json:"relayOn"`
	UpdatedAtMS int64 `json:"updatedAtMs"`
}

type devicePumpDesiredUpdate struct {
	AutoMode    *bool `json:"autoMode,omitempty"`
	RelayOn     *bool `json:"relayOn,omitempty"`
	UpdatedAtMS int64 `json:"updatedAtMs"`
}

func newFirebaseRTDBFromEnv() (*firebaseRTDB, error) {
	databaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("ICARE_FIREBASE_DATABASE_URL")), "/")
	if databaseURL == "" {
		databaseURL = defaultFirebaseDatabaseURL
	}
	parsedURL, err := url.Parse(databaseURL)
	if err != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") || parsedURL.Host == "" || parsedURL.User != nil {
		return nil, errors.New("ICARE_FIREBASE_DATABASE_URL must be an absolute HTTP(S) URL")
	}
	anonymousRead, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("ICARE_FIREBASE_ANONYMOUS_READ")))
	if err != nil && strings.TrimSpace(os.Getenv("ICARE_FIREBASE_ANONYMOUS_READ")) != "" {
		return nil, errors.New("ICARE_FIREBASE_ANONYMOUS_READ must be true or false")
	}
	return &firebaseRTDB{
		databaseURL:   databaseURL,
		authToken:     strings.TrimSpace(os.Getenv("ICARE_FIREBASE_AUTH_TOKEN")),
		apiKey:        strings.TrimSpace(os.Getenv("ICARE_FIREBASE_API_KEY")),
		refreshToken:  strings.TrimSpace(os.Getenv("ICARE_FIREBASE_REFRESH_TOKEN")),
		email:         strings.TrimSpace(os.Getenv("ICARE_FIREBASE_EMAIL")),
		password:      os.Getenv("ICARE_FIREBASE_PASSWORD"),
		anonymousRead: anonymousRead,
		devicesPath:   defaultFirebaseDevicesPath,
		httpClient:    &http.Client{Timeout: 8 * time.Second},
	}, nil
}

func (f *firebaseRTDB) ready() bool {
	if f == nil {
		return false
	}
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	return f.anonymousRead || f.authToken != "" ||
		(f.refreshToken != "" && f.apiKey != "") ||
		(f.email != "" && f.password != "" && f.apiKey != "")
}

func (f *firebaseRTDB) getDevice(ctx context.Context, deviceID string) (deviceSnapshot, error) {
	var snapshot deviceSnapshot
	if !f.ready() {
		return snapshot, errors.New("Firebase ID token or API key and refresh token are required")
	}
	if !validFirebaseKey(deviceID) {
		return snapshot, errors.New("device ID is not a valid Realtime Database key")
	}
	if err := f.readDeviceBranch(ctx, deviceID, "data_terkini", nil, &snapshot.Latest); err != nil {
		return snapshot, err
	}
	if snapshot.Latest == nil {
		return snapshot, errors.New("Firebase device data is missing data_terkini telemetry")
	}
	if err := f.readDeviceBranch(ctx, deviceID, "status_pompa", nil, &snapshot.Pump); err != nil {
		return snapshot, err
	}
	if _, err := f.readOptionalDeviceBranch(ctx, deviceID, "desired", nil, &snapshot.Desired); err != nil {
		return snapshot, err
	}
	if snapshot.Latest.DistanceCM != nil && !finite(*snapshot.Latest.DistanceCM) {
		return snapshot, errors.New("Firebase device data contains a non-finite distance")
	}
	if snapshot.Latest.WaterHeightCM != nil && !finite(*snapshot.Latest.WaterHeightCM) {
		return snapshot, errors.New("Firebase device data contains a non-finite water height")
	}
	if snapshot.Latest.DebitLPM != nil && !finite(*snapshot.Latest.DebitLPM) {
		return snapshot, errors.New("Firebase device data contains a non-finite flow rate")
	}
	return snapshot, nil
}

func (f *firebaseRTDB) canWrite() bool {
	if f == nil {
		return false
	}
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	return !f.anonymousRead && (f.authToken != "" ||
		(f.refreshToken != "" && f.apiKey != "") ||
		(f.email != "" && f.password != "" && f.apiKey != ""))
}

func (f *firebaseRTDB) updateDeviceDesired(ctx context.Context, deviceID string, update devicePumpDesiredUpdate) error {
	if !f.canWrite() {
		return errors.New("Firebase authenticated write credentials are required")
	}
	if !validFirebaseKey(deviceID) {
		return errors.New("device ID is not a valid Realtime Database key")
	}
	if update.AutoMode == nil && update.RelayOn == nil {
		return errors.New("Firebase pump command has no desired state")
	}
	payload, err := json.Marshal(update)
	if err != nil {
		return fmt.Errorf("encode Firebase pump command: %w", err)
	}
	request, err := f.newRequest(ctx, http.MethodPatch, f.deviceBranchURL(deviceID, "desired"), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	response, err := f.httpClient.Do(request)
	if err != nil {
		return errors.New("send Firebase pump command: network request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return firebaseResponseError("write desired pump state", response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}

const firebaseHistoryPageSize = 50

type firebaseHistoryRecord struct {
	Key   string
	Event deviceEvent
}

type firebaseHistoryPage struct {
	Records   []firebaseHistoryRecord
	HasOlder  bool
	HasNewer  bool
	OldestKey string
	NewestKey string
}

func (f *firebaseRTDB) getDeviceHistory(ctx context.Context, deviceID, direction, cursor string) (firebaseHistoryPage, error) {
	var page firebaseHistoryPage
	if !f.ready() {
		return page, errors.New("Firebase ID token or API key and refresh token are required")
	}
	if !validFirebaseKey(deviceID) {
		return page, errors.New("device ID is not a valid Realtime Database key")
	}
	if cursor != "" && !validFirebaseKey(cursor) {
		return page, errors.New("history cursor is not a valid Realtime Database key")
	}
	if direction != "" && direction != "latest" && direction != "before" && direction != "after" {
		return page, errors.New("history direction is invalid")
	}

	query := url.Values{
		"orderBy": {`"$key"`},
	}
	switch direction {
	case "before":
		query.Set("endAt", firebaseJSONString(cursor))
		query.Set("limitToLast", strconv.Itoa(firebaseHistoryPageSize+2))
	case "after":
		query.Set("startAt", firebaseJSONString(cursor))
		query.Set("limitToFirst", strconv.Itoa(firebaseHistoryPageSize+2))
	default:
		query.Set("limitToLast", strconv.Itoa(firebaseHistoryPageSize+1))
	}
	var history map[string]deviceEvent
	if err := f.readDeviceBranch(ctx, deviceID, "riwayat", query, &history); err != nil {
		return page, err
	}

	records := make([]firebaseHistoryRecord, 0, len(history))
	for key, event := range history {
		records = append(records, firebaseHistoryRecord{Key: key, Event: event})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Key < records[j].Key })

	switch direction {
	case "before":
		records = filterHistoryRecords(records, func(record firebaseHistoryRecord) bool { return record.Key < cursor })
		page.HasNewer = cursor != ""
		if len(records) > firebaseHistoryPageSize {
			page.HasOlder = true
			records = records[len(records)-firebaseHistoryPageSize:]
		}
	case "after":
		records = filterHistoryRecords(records, func(record firebaseHistoryRecord) bool { return record.Key > cursor })
		page.HasOlder = cursor != ""
		if len(records) > firebaseHistoryPageSize {
			page.HasNewer = true
			records = records[:firebaseHistoryPageSize]
		}
	default:
		if len(records) > firebaseHistoryPageSize {
			page.HasOlder = true
			records = records[len(records)-firebaseHistoryPageSize:]
		}
	}

	if len(records) > 0 {
		page.OldestKey = records[0].Key
		page.NewestKey = records[len(records)-1].Key
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Event.TimestampMS == records[j].Event.TimestampMS {
			return records[i].Key > records[j].Key
		}
		return records[i].Event.TimestampMS > records[j].Event.TimestampMS
	})
	page.Records = records
	return page, nil
}

func filterHistoryRecords(records []firebaseHistoryRecord, keep func(firebaseHistoryRecord) bool) []firebaseHistoryRecord {
	filtered := make([]firebaseHistoryRecord, 0, len(records))
	for _, record := range records {
		if keep(record) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func firebaseJSONString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (f *firebaseRTDB) readDeviceBranch(ctx context.Context, deviceID, branch string, query url.Values, target any) error {
	found, err := f.readOptionalDeviceBranch(ctx, deviceID, branch, query, target)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Firebase branch %s is missing", branch)
	}
	return nil
}

func (f *firebaseRTDB) readOptionalDeviceBranch(ctx context.Context, deviceID, branch string, query url.Values, target any) (bool, error) {
	requestURL := f.deviceBranchURL(deviceID, branch)
	if len(query) > 0 {
		parsedURL, err := url.Parse(requestURL)
		if err != nil {
			return false, errors.New("create Firebase request: invalid request URL")
		}
		parsedURL.RawQuery = query.Encode()
		requestURL = parsedURL.String()
	}
	request, err := f.newRequest(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return false, err
	}
	response, err := f.httpClient.Do(request)
	if err != nil {
		return false, fmt.Errorf("read Firebase %s: network request failed", branch)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, firebaseResponseError("read "+branch, response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return false, fmt.Errorf("decode Firebase %s data: %w", branch, err)
	}
	return true, nil
}

func (f *firebaseRTDB) deviceURL(deviceID string) string {
	return f.databaseURL + "/" + f.devicesPath + "/" + url.PathEscape(deviceID) + ".json"
}

func (f *firebaseRTDB) deviceBranchURL(deviceID, branch string) string {
	return f.databaseURL + "/" + f.devicesPath + "/" + url.PathEscape(deviceID) + "/" + branch + ".json"
}

type firebaseStreamEvent struct {
	Type string          `json:"-"`
	Path string          `json:"path"`
	Data json.RawMessage `json:"data"`
}

func (f *firebaseRTDB) streamDeviceBranch(ctx context.Context, deviceID, branch string, query url.Values, callback func(firebaseStreamEvent) error) error {
	requestURL := f.deviceBranchURL(deviceID, branch)
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return errors.New("create Firebase stream request: invalid request URL")
	}
	parsedURL.RawQuery = query.Encode()
	request, err := f.newRequest(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/event-stream")
	baseClient := f.httpClient
	if baseClient == nil {
		baseClient = http.DefaultClient
	}
	client := *baseClient
	client.Timeout = 0
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("connect Firebase %s stream: network request failed", branch)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return firebaseResponseError("stream "+branch, response)
	}

	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	eventType := ""
	var data strings.Builder
	dispatch := func() error {
		if data.Len() == 0 {
			eventType = ""
			return nil
		}
		var event firebaseStreamEvent
		if err := json.Unmarshal([]byte(data.String()), &event); err != nil {
			return fmt.Errorf("decode Firebase %s stream event: %w", branch, err)
		}
		event.Type = eventType
		data.Reset()
		eventType = ""
		if event.Type == "keep-alive" || event.Type == "" {
			return nil
		}
		return callback(event)
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := scanner.Text()
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("read Firebase %s stream: %w", branch, err)
	}
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("Firebase %s stream closed unexpectedly", branch)
}

func (f *firebaseRTDB) newRequest(ctx context.Context, method, requestURL string, body io.Reader) (*http.Request, error) {
	token, err := f.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, errors.New("create Firebase request: invalid request URL")
	}
	query := parsedURL.Query()
	if token != "" {
		query.Set("auth", token)
	}
	parsedURL.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), body)
	if err != nil {
		return nil, errors.New("create Firebase request: invalid request URL")
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request, nil
}

func (f *firebaseRTDB) accessToken(ctx context.Context) (string, error) {
	f.tokenMu.Lock()
	defer f.tokenMu.Unlock()
	if f.anonymousRead {
		return "", nil
	}
	if f.refreshToken == "" && f.apiKey != "" && f.email != "" && f.password != "" {
		return f.signInWithPasswordLocked(ctx)
	}
	if f.refreshToken == "" || f.apiKey == "" {
		if f.authToken != "" {
			return f.authToken, nil
		}
		if f.refreshToken != "" {
			return "", errors.New("ICARE_FIREBASE_API_KEY is required with a refresh token")
		}
		return "", errors.New("Firebase ID token or API key and refresh token are required")
	}

	if f.authToken != "" && time.Now().Add(time.Minute).Before(f.tokenExpiry) {
		return f.authToken, nil
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {f.refreshToken},
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://securetoken.googleapis.com/v1/token?key="+url.QueryEscape(f.apiKey),
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", fmt.Errorf("create Firebase token refresh request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := f.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("refresh Firebase access token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", firebaseResponseError("token refresh", response)
	}
	var result struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    string `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Firebase token response: %w", err)
	}
	seconds, err := strconv.Atoi(result.ExpiresIn)
	if err != nil || result.IDToken == "" || seconds <= 0 {
		return "", errors.New("Firebase returned an invalid token refresh response")
	}
	f.authToken = result.IDToken
	f.tokenExpiry = time.Now().Add(time.Duration(seconds) * time.Second)
	if result.RefreshToken != "" {
		f.refreshToken = result.RefreshToken
	}
	return f.authToken, nil
}

// signInWithPasswordLocked masuk ke Firebase Auth memakai email+password akun
// yang UID-nya diizinkan oleh Realtime Database Rules. Token di-cache dan
// setelah itu diperbarui lewat refresh token. Pemanggil harus memegang tokenMu.
func (f *firebaseRTDB) signInWithPasswordLocked(ctx context.Context) (string, error) {
	if f.authToken != "" && time.Now().Add(time.Minute).Before(f.tokenExpiry) {
		return f.authToken, nil
	}
	payload, err := json.Marshal(map[string]any{
		"email":             f.email,
		"password":          f.password,
		"returnSecureToken": true,
	})
	if err != nil {
		return "", fmt.Errorf("create Firebase sign-in request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://identitytoolkit.googleapis.com/v1/accounts:signInWithPassword?key="+url.QueryEscape(f.apiKey),
		strings.NewReader(string(payload)),
	)
	if err != nil {
		return "", fmt.Errorf("create Firebase sign-in request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := f.httpClient.Do(request)
	if err != nil {
		return "", errors.New("Firebase sign-in: network request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", firebaseResponseError("sign-in", response)
	}
	var result struct {
		IDToken      string `json:"idToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    string `json:"expiresIn"`
		LocalID      string `json:"localId"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Firebase sign-in response: %w", err)
	}
	seconds, err := strconv.Atoi(result.ExpiresIn)
	if err != nil || result.IDToken == "" || seconds <= 0 {
		return "", errors.New("Firebase returned an invalid sign-in response")
	}
	f.authToken = result.IDToken
	f.tokenExpiry = time.Now().Add(time.Duration(seconds) * time.Second)
	if result.RefreshToken != "" {
		f.refreshToken = result.RefreshToken
	}
	log.Printf("Firebase sign-in berhasil (uid %s)", result.LocalID)
	return f.authToken, nil
}

func firebaseResponseError(operation string, response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	if len(message) == 0 {
		return fmt.Errorf("Firebase %s returned HTTP %d", operation, response.StatusCode)
	}
	return fmt.Errorf("Firebase %s returned HTTP %d: %s", operation, response.StatusCode, strings.TrimSpace(string(message)))
}

func validFirebaseKey(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || strings.ContainsRune(".#$[]/", character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
