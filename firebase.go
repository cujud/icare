package main

import (
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
	SessionID      string `json:"id_sesi"`
	StatusReason   string `json:"keterangan_status"`
	ConfigActive   bool   `json:"konfigurasi_aktif"`
	Mode           string `json:"mode_operasi"`
	PumpOn         *bool  `json:"pompa_menyala"`
	ConfigRevision int    `json:"revisi_konfigurasi"`
	SensorReady    bool   `json:"sensor_siap"`
	UpdatedAtMS    int64  `json:"waktu_update_server"`
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
	request, err := f.newRequest(ctx, http.MethodGet, f.deviceURL(deviceID), nil)
	if err != nil {
		return snapshot, err
	}
	response, err := f.httpClient.Do(request)
	if err != nil {
		return snapshot, errors.New("read Firebase device: network request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return snapshot, firebaseResponseError("read", response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&snapshot); err != nil {
		return snapshot, fmt.Errorf("decode Firebase device data: %w", err)
	}
	if snapshot.Latest == nil {
		return snapshot, errors.New("Firebase device data is missing data_terkini telemetry")
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

func (f *firebaseRTDB) deviceURL(deviceID string) string {
	return f.databaseURL + "/" + f.devicesPath + "/" + url.PathEscape(deviceID) + ".json"
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
