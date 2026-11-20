package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	_ "modernc.org/sqlite"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	sessionCookieName = "icare_session"
	sessionDuration   = 12 * time.Hour
	passwordRounds    = 120_000
)

type User struct {
	Name         string
	Email        string
	Phone        string
	PasswordSalt []byte
	PasswordHash []byte
}

type Session struct {
	Email     string
	ExpiresAt time.Time
}

type Activity struct {
	Key                  string  `json:"key,omitempty"`
	Time                 string  `json:"time"`
	Description          string  `json:"description"`
	Mode                 string  `json:"mode"`
	TimestampMS          int64   `json:"timestamp_ms,omitempty"`
	WaterHeightCM        float64 `json:"water_height_cm"`
	WaterHeightAvailable bool    `json:"water_height_available,omitempty"`
}

type Plot struct {
	Name     string
	DeviceID string
}

type IrrigationState struct {
	WaterLevel float64
	FlowRate   float64
	Pressure   float64
	PumpOn     bool
	Mode       string
	Events     []Activity
}

type PageData struct {
	Title                  string
	Active                 string
	Petak                  string
	Petaks                 []Plot
	DeviceID               string
	LinkedDeviceID         string
	FirebaseConnected      bool
	DeviceError            string
	DeviceNode             string
	DeviceFault            string
	SensorStatus           string
	SensorAvailable        bool
	SensorValid            bool
	DataFresh              bool
	DistanceCM             float64
	WaterHeightCM          float64
	WaterHeightAvailable   bool
	FlowCalibrated         bool
	DebitAvailable         bool
	DebitLPM               float64
	TotalLiter             float64
	TotalAvailable         bool
	RSSI                   int
	RSSIAvailable          bool
	LastUpdateMS           int64
	LastUpdate             string
	LastUpdateAgo          string
	DeviceStale            bool
	LoRaOnline             bool
	SensorSeen             bool
	PumpStatusAvailable    bool
	DeviceWriteAvailable   bool
	DesiredStateAvailable  bool
	DeviceControlAvailable bool
	DeviceControlPending   bool
	DesiredUpdatedAtMS     int64
	AutoThresholdAvailable bool
	AutoOffDistanceCM      float64
	AutoOnDistanceCM       float64
	AutoChartThresholds    bool
	AutoOffChartY          float64
	AutoOnChartY           float64
	ConfigFresh            bool
	ConfigReason           string
	PlotName               string
	IsAuth                 bool
	Error                  string
	UserName               string
	UserEmail              string
	Initials               string
	Today                  string
	WaterLevel             float64
	FlowRate               float64
	FlowPercent            float64
	Pressure               float64
	PressureLabel          string
	PumpOn                 bool
	Mode                   string
	ModeLabel              string
	ChartPoints            string
	ChartStartMS           int64
	ChartLabels            []string
	Events                 []Activity
	HistoryPage            int
	OlderPage              int
	NewerPage              int
	HistoryPageSize        int
	HistoryOlderCursor     string
	HistoryNewerCursor     string
	HasOlderHistory        bool
	HasNewerHistory        bool
}

type App struct {
	mux           *http.ServeMux
	templates     map[string]*template.Template
	mu            sync.Mutex
	sessions      map[string]Session
	db            *sql.DB
	state         IrrigationState
	firebase      *firebaseRTDB
	pumpCommandMu sync.Mutex
	// defaultDeviceID dipakai untuk petak yang belum punya ID perangkat sendiri
	// (ICARE_DEFAULT_DEVICE_ID), sehingga dashboard tidak jatuh ke data demo.
	defaultDeviceID string
}

func newApp() (*App, error) {
	dbPath := strings.TrimSpace(os.Getenv("ICARE_DB_PATH"))
	if dbPath == "" {
		dbPath = "icare.db"
	}
	if dbPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dbPath), 0700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := initializeDatabase(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	app, err := newAppWithDB(db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return app, nil
}

func newAppWithDB(db *sql.DB) (*App, error) {
	firebase, err := newFirebaseRTDBFromEnv()
	if err != nil {
		return nil, err
	}
	return newAppWithDBAndFirebase(db, firebase)
}

func newAppWithDBAndFirebase(db *sql.DB, firebase *firebaseRTDB) (*App, error) {
	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load static assets: %w", err)
	}

	app := &App{
		mux:             http.NewServeMux(),
		templates:       make(map[string]*template.Template),
		sessions:        make(map[string]Session),
		db:              db,
		firebase:        firebase,
		defaultDeviceID: strings.TrimSpace(os.Getenv("ICARE_DEFAULT_DEVICE_ID")),
		state: IrrigationState{
			WaterLevel: 75,
			FlowRate:   85,
			Pressure:   1.2,
			PumpOn:     true,
			Mode:       "auto",
			Events: []Activity{
				{Time: "08.15", Description: "Pompa menyala", Mode: "Otomatis"},
				{Time: "06.40", Description: "Pompa mati, air mencapai 80 cm", Mode: "Otomatis"},
				{Time: "05.10", Description: "Pompa menyala", Mode: "Manual"},
			},
		},
	}

	pageFiles := map[string]string{
		"dashboard": "dashboard.html",
		"login":     "login.html",
		"register":  "register.html",
		"history":   "history.html",
		"devices":   "devices.html",
		"profile":   "profile.html",
	}
	for name, file := range pageFiles {
		tmpl, err := template.ParseFS(assets, "templates/layout.html", "templates/"+file)
		if err != nil {
			return nil, fmt.Errorf("parse %s template: %w", name, err)
		}
		app.templates[name] = tmpl
	}

	app.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))
	app.mux.HandleFunc("GET /healthz", app.health)
	app.mux.HandleFunc("GET /{$}", app.dashboard)
	app.mux.HandleFunc("GET /login", app.loginPage)
	app.mux.HandleFunc("POST /login", app.login)
	app.mux.HandleFunc("GET /register", app.registerPage)
	app.mux.HandleFunc("POST /register", app.register)
	app.mux.HandleFunc("POST /logout", app.logout)
	app.mux.HandleFunc("GET /history", app.history)
	app.mux.HandleFunc("GET /devices", app.devices)
	app.mux.HandleFunc("GET /profile", app.profile)
	app.mux.HandleFunc("POST /plots", app.addPlot)
	app.mux.HandleFunc("POST /pump/mode", app.setPumpMode)
	app.mux.HandleFunc("POST /pump/toggle", app.togglePump)
	app.mux.HandleFunc("GET /ws/device", app.deviceWebSocket)

	return app, nil
}

func initializeDatabase(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS users (
	email TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	phone TEXT NOT NULL,
	password_salt BLOB NOT NULL,
	password_hash BLOB NOT NULL,
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS petaks (
	id INTEGER PRIMARY KEY,
	user_email TEXT NOT NULL REFERENCES users(email) ON DELETE CASCADE,
	name TEXT NOT NULL,
	device_id TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(user_email, name)
);
`
	if _, err := db.Exec("PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;"); err != nil {
		return fmt.Errorf("configure SQLite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("initialize SQLite schema: %w", err)
	}
	return nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

type deviceRealtimeMessage struct {
	Type     string          `json:"type"`
	Path     string          `json:"path,omitempty"`
	Data     json.RawMessage `json:"data,omitempty"`
	Activity *Activity       `json:"activity,omitempty"`
	Message  string          `json:"message,omitempty"`
}

var deviceWebSocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func (a *App) deviceWebSocket(w http.ResponseWriter, r *http.Request) {
	user, authenticated, err := a.currentUser(r)
	if err != nil {
		http.Error(w, "Akun tidak dapat diperiksa. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if !authenticated {
		http.Error(w, "Sesi login diperlukan.", http.StatusUnauthorized)
		return
	}
	plot, hasDevice, err := a.selectedDevicePlot(user, r)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Petak tidak dapat diperiksa.", http.StatusInternalServerError)
		return
	}
	if !hasDevice || a.firebase == nil || !a.firebase.ready() {
		http.Error(w, "Perangkat Firebase tidak tersedia.", http.StatusServiceUnavailable)
		return
	}

	conn, err := deviceWebSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(1024)
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	updates := make(chan deviceRealtimeMessage, 16)
	streamErrors := make(chan error, 3)
	enqueue := func(message deviceRealtimeMessage) error {
		select {
		case updates <- message:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	startStream := func(branch string, query url.Values, handle func(firebaseStreamEvent) error) {
		go func() {
			if err := a.firebase.streamDeviceBranch(ctx, plot.DeviceID, branch, query, handle); err != nil && ctx.Err() == nil {
				select {
				case streamErrors <- fmt.Errorf("Firebase %s stream: %w", branch, err):
				case <-ctx.Done():
				}
			}
		}()
	}

	telemetryHandler := func(messageType string) func(firebaseStreamEvent) error {
		return func(event firebaseStreamEvent) error {
			if event.Type == "cancel" || event.Type == "auth_revoked" {
				return fmt.Errorf("Firebase %s stream was cancelled", messageType)
			}
			if event.Type != "put" && event.Type != "patch" {
				return nil
			}
			return enqueue(deviceRealtimeMessage{Type: messageType, Path: event.Path, Data: event.Data})
		}
	}
	startStream("data_terkini", nil, telemetryHandler("telemetry"))
	startStream("status_pompa", nil, telemetryHandler("pump"))

	historyQuery := url.Values{
		"orderBy":     {`"$key"`},
		"limitToLast": {"1"},
	}
	startStream("riwayat", historyQuery, func(event firebaseStreamEvent) error {
		if event.Type == "cancel" || event.Type == "auth_revoked" {
			return errors.New("Firebase history stream was cancelled")
		}
		if event.Type != "put" && event.Type != "patch" {
			return nil
		}
		activity, ok := activityFromStreamEvent(event)
		if !ok {
			return nil
		}
		return enqueue(deviceRealtimeMessage{Type: "history", Activity: &activity})
	})

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case message := <-updates:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteJSON(message); err != nil {
				return
			}
		case err := <-streamErrors:
			log.Printf("device realtime stream %q: %v", plot.DeviceID, err)
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = conn.WriteJSON(deviceRealtimeMessage{Type: "error", Message: "Koneksi realtime Firebase terputus."})
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		}
	}
}

func activityFromStreamEvent(event firebaseStreamEvent) (Activity, bool) {
	var records map[string]deviceEvent
	switch {
	case event.Path == "/" || event.Path == "":
		if err := json.Unmarshal(event.Data, &records); err != nil {
			return Activity{}, false
		}
	case strings.Count(strings.Trim(event.Path, "/"), "/") == 0:
		key := strings.Trim(event.Path, "/")
		var record deviceEvent
		if err := json.Unmarshal(event.Data, &record); err != nil {
			return Activity{}, false
		}
		return activityFromDeviceEvent(key, record), true
	default:
		return Activity{}, false
	}
	var key string
	for candidate := range records {
		if candidate > key {
			key = candidate
		}
	}
	if key == "" {
		return Activity{}, false
	}
	return activityFromDeviceEvent(key, records[key]), true
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	data, err := a.pageData(user, "Beranda")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	if !selectPetak(&data, r.URL.Query().Get("petak")) {
		http.NotFound(w, r)
		return
	}
	a.loadDeviceData(r.Context(), &data, "latest", "")
	a.render(w, "dashboard", data)
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	_, ok, err := a.currentUser(r)
	if err != nil {
		http.Error(w, "Akun tidak dapat diperiksa. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "login", PageData{Title: "Masuk", IsAuth: true})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}

	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	user, err := a.getUser(email)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Akun tidak dapat diperiksa. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if !exists || !checkPassword(user.PasswordSalt, user.PasswordHash, password) {
		a.render(w, "login", PageData{
			Title:  "Masuk",
			IsAuth: true,
			Error:  "Email atau kata sandi tidak sesuai.",
		})
		return
	}

	if err := a.startSession(w, r, user.Email); err != nil {
		http.Error(w, "Sesi tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) registerPage(w http.ResponseWriter, r *http.Request) {
	_, ok, err := a.currentUser(r)
	if err != nil {
		http.Error(w, "Akun tidak dapat diperiksa. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true})
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	email := normalizeEmail(r.FormValue("email"))
	phone := strings.TrimSpace(r.FormValue("phone"))
	password := r.FormValue("password")
	confirmation := r.FormValue("password_confirmation")

	switch {
	case name == "" || !utf8.ValidString(name):
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Nama lengkap wajib diisi."})
		return
	case !validEmail(email):
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Masukkan alamat email yang valid."})
		return
	case phone == "":
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Nomor HP wajib diisi."})
		return
	case len(password) < 8:
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Kata sandi harus memiliki minimal 8 karakter."})
		return
	case password != confirmation:
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Konfirmasi kata sandi belum cocok."})
		return
	case r.FormValue("terms") != "on":
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Setujui syarat dan kebijakan privasi untuk melanjutkan."})
		return
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		http.Error(w, "Akun tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	user := User{
		Name:         name,
		Email:        email,
		Phone:        phone,
		PasswordSalt: salt,
		PasswordHash: pbkdf2SHA256([]byte(password), salt, passwordRounds, sha256.Size),
	}

	tx, err := a.db.Begin()
	if err != nil {
		http.Error(w, "Akun tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	result, err := tx.Exec(
		"INSERT INTO users (email, name, phone, password_salt, password_hash) VALUES (?, ?, ?, ?, ?) ON CONFLICT(email) DO NOTHING",
		user.Email, user.Name, user.Phone, user.PasswordSalt, user.PasswordHash,
	)
	if err != nil {
		_ = tx.Rollback()
		http.Error(w, "Akun tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		http.Error(w, "Akun tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if inserted == 0 {
		_ = tx.Rollback()
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Email ini sudah terdaftar."})
		return
	}
	if _, err := tx.Exec("INSERT INTO petaks (user_email, name) VALUES (?, ?)", email, "Petak A"); err != nil {
		_ = tx.Rollback()
		http.Error(w, "Petak awal tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "Akun tidak dapat dibuat. Coba lagi.", http.StatusInternalServerError)
		return
	}

	if err := a.startSession(w, r, email); err != nil {
		http.Error(w, "Akun dibuat, tetapi sesi tidak dapat dimulai. Silakan masuk.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *App) history(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	data, err := a.pageData(user, "Riwayat")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	if !selectPetak(&data, r.URL.Query().Get("petak")) {
		http.NotFound(w, r)
		return
	}
	before := r.URL.Query().Get("before")
	after := r.URL.Query().Get("after")
	if before != "" && after != "" {
		http.Error(w, "Gunakan satu cursor riwayat saja.", http.StatusBadRequest)
		return
	}
	if (before != "" && !validFirebaseKey(before)) || (after != "" && !validFirebaseKey(after)) {
		http.Error(w, "Cursor riwayat tidak valid.", http.StatusBadRequest)
		return
	}
	page := 1
	if rawPage := r.URL.Query().Get("page"); rawPage != "" {
		parsedPage, err := strconv.Atoi(rawPage)
		if err != nil || parsedPage < 1 {
			http.Error(w, "Halaman riwayat tidak valid.", http.StatusBadRequest)
			return
		}
		page = parsedPage
	}
	direction, cursor := "latest", ""
	if before != "" {
		direction, cursor = "before", before
	} else if after != "" {
		direction, cursor = "after", after
	}
	data.HistoryPage = page
	data.HistoryPageSize = firebaseHistoryPageSize
	a.loadDeviceData(r.Context(), &data, direction, cursor)
	a.render(w, "history", data)
}

func (a *App) devices(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	data, err := a.pageData(user, "Perangkat")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	if !selectPetak(&data, r.URL.Query().Get("petak")) {
		http.NotFound(w, r)
		return
	}
	a.loadDeviceData(r.Context(), &data, "", "")
	a.render(w, "devices", data)
}

func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	data, err := a.pageData(user, "Profil")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	a.render(w, "profile", data)
}

func (a *App) addPlot(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	deviceID := strings.TrimSpace(r.FormValue("device_id"))
	if name == "" || !utf8.ValidString(name) || len(name) > 50 || len(deviceID) > 128 {
		data, err := a.pageData(user, "Beranda")
		if err != nil {
			http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
			return
		}
		data.Error = "Nama petak wajib diisi (maksimal 50 karakter); ID perangkat maksimal 128 karakter."
		data.PlotName = name
		data.DeviceID = deviceID
		if selected := r.FormValue("petak"); selected != "" {
			_ = selectPetak(&data, selected)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		a.render(w, "dashboard", data)
		return
	}
	result, err := a.db.Exec(
		"INSERT INTO petaks (user_email, name, device_id) VALUES (?, ?, ?) ON CONFLICT(user_email, name) DO NOTHING",
		user.Email, name, deviceID,
	)
	if err != nil {
		http.Error(w, "Petak tidak dapat disimpan. Coba lagi.", http.StatusInternalServerError)
		return
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "Petak tidak dapat disimpan. Coba lagi.", http.StatusInternalServerError)
		return
	}
	if inserted == 0 {
		data, err := a.pageData(user, "Beranda")
		if err != nil {
			http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
			return
		}
		data.Error = "Nama petak tersebut sudah digunakan."
		data.PlotName = name
		data.DeviceID = deviceID
		w.WriteHeader(http.StatusConflict)
		a.render(w, "dashboard", data)
		return
	}
	http.Redirect(w, r, "/?petak="+url.QueryEscape(name), http.StatusSeeOther)
}

func (a *App) setPumpMode(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}
	mode := r.FormValue("mode")
	if mode != "auto" && mode != "manual" {
		http.Error(w, "Mode pompa tidak valid.", http.StatusBadRequest)
		return
	}
	plot, hasDevice, err := a.selectedDevicePlot(user, r)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Petak tidak dapat diperiksa.", http.StatusInternalServerError)
		return
	}
	if hasDevice {
		a.pumpCommandMu.Lock()
		defer a.pumpCommandMu.Unlock()
		a.sendFirebasePumpCommand(w, r, plot, devicePumpDesiredUpdate{
			AutoMode:    boolPointer(mode == "auto"),
			UpdatedAtMS: time.Now().UnixMilli(),
		}, mode, nil, false, nil)
		return
	}

	a.mu.Lock()
	if a.state.Mode != mode {
		a.state.Mode = mode
		label := modeLabel(mode)
		a.addEventLocked("Mode pompa diubah ke "+label, label)
	}
	if mode == "auto" {
		a.applyAutomaticControlLocked()
	}
	a.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) togglePump(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	plot, hasDevice, err := a.selectedDevicePlot(user, r)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "Petak tidak dapat diperiksa.", http.StatusInternalServerError)
		return
	}
	if hasDevice {
		a.pumpCommandMu.Lock()
		defer a.pumpCommandMu.Unlock()
		if a.firebase == nil || !a.firebase.canWrite() {
			a.pumpCommandError(w, r, http.StatusServiceUnavailable, "Akun Firebase tidak memiliki akses tulis untuk kontrol pompa.")
			return
		}
		snapshot, err := a.firebase.getDevice(r.Context(), plot.DeviceID)
		if err != nil {
			log.Printf("read Firebase pump state %q before command: %v", plot.DeviceID, err)
			a.pumpCommandError(w, r, http.StatusBadGateway, "Status pompa tidak dapat diverifikasi dari Firebase.")
			return
		}
		if snapshot.Desired == nil || snapshot.Desired.AutoMode == nil || snapshot.Desired.RelayOn == nil {
			a.pumpCommandError(w, r, http.StatusConflict, "Node devices/"+plot.DeviceID+"/desired belum memiliki autoMode dan relayOn.")
			return
		}
		if snapshot.Pump == nil || snapshot.Pump.UpdatedAtMS <= snapshot.Desired.UpdatedAtMS {
			a.pumpCommandError(w, r, http.StatusConflict, "Perangkat belum mengonfirmasi perintah sebelumnya dari Firebase.")
			return
		}
		if snapshot.Pump == nil || normalizeDeviceMode(snapshot.Pump.Mode) != "manual" {
			a.pumpCommandError(w, r, http.StatusConflict, "Ubah pompa ke mode manual dan tunggu konfirmasi Firebase sebelum mengubah status relay.")
			return
		}
		var currentPump *bool
		if snapshot.Pump != nil {
			currentPump = snapshot.Pump.PumpOn
		}
		if currentPump == nil && snapshot.Latest != nil {
			currentPump = snapshot.Latest.PumpOn
		}
		if currentPump == nil {
			a.pumpCommandError(w, r, http.StatusConflict, "Status pompa belum tersedia di Firebase.")
			return
		}
		targetPump := !*currentPump
		a.sendFirebasePumpCommand(w, r, plot, devicePumpDesiredUpdate{
			AutoMode:    boolPointer(false),
			RelayOn:     boolPointer(targetPump),
			UpdatedAtMS: time.Now().UnixMilli(),
		}, "manual", boolPointer(targetPump), true, &snapshot)
		return
	}
	a.mu.Lock()
	a.state.PumpOn = !a.state.PumpOn
	description := "Pompa mati"
	if a.state.PumpOn {
		description = "Pompa menyala"
	}
	a.addEventLocked(description, modeLabel(a.state.Mode))
	a.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

type pumpCommandResult struct {
	Status         string `json:"status"`
	UpdatedAtMS    int64  `json:"updatedAtMs"`
	ExpectedMode   string `json:"expectedMode"`
	ExpectedPumpOn *bool  `json:"expectedPumpOn,omitempty"`
}

func (a *App) sendFirebasePumpCommand(w http.ResponseWriter, r *http.Request, plot Plot, update devicePumpDesiredUpdate, expectedMode string, expectedPump *bool, requireManualMode bool, snapshot *deviceSnapshot) {
	if a.firebase == nil || !a.firebase.canWrite() {
		a.pumpCommandError(w, r, http.StatusServiceUnavailable, "Akun Firebase tidak memiliki akses tulis untuk kontrol pompa.")
		return
	}
	if snapshot == nil {
		loaded, loadErr := a.firebase.getDevice(r.Context(), plot.DeviceID)
		if loadErr != nil {
			log.Printf("read Firebase desired pump state %q: %v", plot.DeviceID, loadErr)
			a.pumpCommandError(w, r, http.StatusBadGateway, "Status perangkat tidak dapat diverifikasi dari Firebase.")
			return
		}
		snapshot = &loaded
	}
	if snapshot.Desired == nil || snapshot.Desired.AutoMode == nil || snapshot.Desired.RelayOn == nil {
		a.pumpCommandError(w, r, http.StatusConflict, "Node devices/"+plot.DeviceID+"/desired belum memiliki autoMode dan relayOn.")
		return
	}
	if snapshot.Pump == nil || snapshot.Pump.UpdatedAtMS <= snapshot.Desired.UpdatedAtMS {
		a.pumpCommandError(w, r, http.StatusConflict, "Perangkat belum mengonfirmasi perintah sebelumnya dari Firebase.")
		return
	}
	if expectedMode == "manual" && expectedPump == nil {
		currentPump := snapshot.Latest.PumpOn
		if currentPump == nil && snapshot.Pump != nil {
			currentPump = snapshot.Pump.PumpOn
		}
		if currentPump == nil {
			a.pumpCommandError(w, r, http.StatusConflict, "Status pompa belum tersedia untuk mempertahankan kondisi relay saat beralih ke mode manual.")
			return
		}
		update.RelayOn = boolPointer(*currentPump)
		expectedPump = boolPointer(*currentPump)
	}
	if requireManualMode {
		if snapshot.Pump == nil || normalizeDeviceMode(snapshot.Pump.Mode) != "manual" {
			a.pumpCommandError(w, r, http.StatusConflict, "Mode perangkat telah berubah; status pompa harus dikonfirmasi dalam mode manual.")
			return
		}
		var currentPump *bool
		if snapshot.Pump != nil {
			currentPump = snapshot.Pump.PumpOn
		}
		if currentPump == nil && snapshot.Latest != nil {
			currentPump = snapshot.Latest.PumpOn
		}
		if expectedPump == nil || currentPump == nil || *currentPump == *expectedPump {
			a.pumpCommandError(w, r, http.StatusConflict, "Status pompa telah berubah. Muat ulang dashboard sebelum mengirim perintah.")
			return
		}
	}
	if err := a.firebase.updateDeviceDesired(r.Context(), plot.DeviceID, update); err != nil {
		log.Printf("write Firebase desired pump state %q: %v", plot.DeviceID, err)
		a.pumpCommandError(w, r, http.StatusBadGateway, "Perintah pompa gagal dikirim ke Firebase.")
		return
	}
	result := pumpCommandResult{
		Status:         "pending",
		UpdatedAtMS:    update.UpdatedAtMS,
		ExpectedMode:   expectedMode,
		ExpectedPumpOn: expectedPump,
	}
	if acceptsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		if err := json.NewEncoder(w).Encode(result); err != nil {
			log.Printf("write pump command response: %v", err)
		}
		return
	}
	location := "/"
	if r.FormValue("petak") != "" {
		location += "?petak=" + url.QueryEscape(r.FormValue("petak"))
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (a *App) pumpCommandError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if acceptsJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": message}); err != nil {
			log.Printf("write pump command error response: %v", err)
		}
		return
	}
	http.Error(w, message, status)
}

func acceptsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func boolPointer(value bool) *bool {
	return &value
}

func normalizeDeviceMode(mode string) string {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "MANUAL":
		return "manual"
	case "AUTO", "OTOMATIS":
		return "auto"
	default:
		return ""
	}
}

func (a *App) currentUser(r *http.Request) (User, bool, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return User{}, false, nil
	}

	a.mu.Lock()
	session, exists := a.sessions[cookie.Value]
	if !exists || time.Now().After(session.ExpiresAt) {
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
		return User{}, false, nil
	}
	a.mu.Unlock()
	user, err := a.getUser(session.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return user, true, nil
}

func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, ok, err := a.currentUser(r)
	if err != nil {
		http.Error(w, "Akun tidak dapat diperiksa. Coba lagi.", http.StatusInternalServerError)
		return User{}, false
	}
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return User{}, false
	}
	return user, true
}

func (a *App) startSession(w http.ResponseWriter, r *http.Request, email string) error {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return err
	}
	value := hex.EncodeToString(token)
	expiresAt := time.Now().Add(sessionDuration)

	a.mu.Lock()
	now := time.Now()
	for id, session := range a.sessions {
		if now.After(session.ExpiresAt) {
			delete(a.sessions, id)
		}
	}
	a.sessions[value] = Session{Email: email, ExpiresAt: expiresAt}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	})
	return nil
}

func (a *App) pageData(user User, active string) (PageData, error) {
	a.mu.Lock()
	state := a.state
	events := append([]Activity(nil), state.Events...)
	a.mu.Unlock()
	plots, err := a.listPlots(user.Email)
	if err != nil {
		return PageData{}, err
	}

	name := user.Name
	if name == "" {
		name = "User"
	}
	chartNow := time.Now()
	data := PageData{
		Title:       active,
		Active:      active,
		Petaks:      plots,
		UserName:    name,
		UserEmail:   user.Email,
		Initials:    initials(name),
		Today:       indonesianDate(chartNow),
		WaterLevel:  state.WaterLevel,
		FlowRate:    state.FlowRate,
		FlowPercent: state.FlowRate / 120 * 100,
		Pressure:    state.Pressure,
		PressureLabel: strings.ReplaceAll(
			strconv.FormatFloat(state.Pressure, 'f', 1, 64),
			".",
			",",
		),
		PumpOn:       state.PumpOn,
		Mode:         state.Mode,
		ModeLabel:    modeLabel(state.Mode),
		ChartPoints:  makeChartPoints([]float64{43, 58, 74, 64, 87, 93, 81, 100, 96, state.WaterLevel}),
		ChartStartMS: chartNow.Add(-24 * time.Hour).UnixMilli(),
		ChartLabels:  chartTimeLabels(chartNow),
		Events:       events,
	}
	if len(plots) > 0 {
		data.Petak = plots[0].Name
	}
	return data, nil
}

func (a *App) loadDeviceData(ctx context.Context, data *PageData, historyDirection, historyCursor string) {
	plot := findPlot(data.Petaks, data.Petak)
	if plot.DeviceID == "" {
		return
	}
	data.LinkedDeviceID = plot.DeviceID
	data.Events = nil
	data.ChartPoints = ""
	if a.firebase == nil || !a.firebase.ready() {
		data.DeviceError = "Akses Firebase membutuhkan token autentikasi; data perangkat tidak tersedia."
		return
	}
	snapshot, err := a.firebase.getDevice(ctx, plot.DeviceID)
	if err != nil {
		log.Printf("read Firebase device %q: %v", plot.DeviceID, err)
		data.DeviceError = "Data perangkat tidak dapat dimuat dari Firebase. Periksa token dan rules Realtime Database."
		return
	}
	data.FirebaseConnected = true
	latest := snapshot.Latest
	data.DeviceNode = latest.Node
	data.DeviceFault = latest.Fault
	data.SensorStatus = latest.SensorStatus
	data.FlowCalibrated = latest.FlowCalibrated
	data.LoRaOnline = latest.LoRaOnline
	data.SensorSeen = latest.SensorConnected
	data.SensorValid = latest.SensorValid
	data.DataFresh = latest.DataFresh
	if latest.DistanceCM != nil && *latest.DistanceCM >= 0 && latest.SensorConnected && latest.SensorValid && latest.DataFresh {
		data.SensorAvailable = true
		data.DistanceCM = *latest.DistanceCM
	}
	if latest.WaterHeightCM != nil && *latest.WaterHeightCM >= 0 && latest.SensorConnected && latest.SensorValid && latest.DataFresh {
		data.WaterHeightAvailable = true
		data.WaterHeightCM = *latest.WaterHeightCM
	}
	if latest.DebitLPM != nil && finite(*latest.DebitLPM) && *latest.DebitLPM >= 0 {
		data.DebitAvailable = true
		data.DebitLPM = *latest.DebitLPM
	}
	if latest.TotalLiter != nil && finite(*latest.TotalLiter) && *latest.TotalLiter >= 0 {
		data.TotalAvailable = true
		data.TotalLiter = *latest.TotalLiter
	}
	if latest.RSSI != nil {
		data.RSSIAvailable = true
		data.RSSI = *latest.RSSI
	}
	uploaded := latest.UploadedAtMS
	if uploaded <= 0 {
		uploaded = latest.ReceivedAtMS
	}
	if uploaded > 0 {
		data.LastUpdateMS = uploaded
		uploadedAt := time.UnixMilli(uploaded)
		data.LastUpdate = uploadedAt.In(time.FixedZone("WITA", 8*60*60)).Format("02/01/2006 15.04.05") + " WITA"
		age := time.Since(uploadedAt)
		switch {
		case age < 0:
			data.LastUpdateAgo = "baru saja"
		case age < time.Minute:
			data.LastUpdateAgo = fmt.Sprintf("%d detik lalu", int(age.Seconds()))
		case age < time.Hour:
			data.LastUpdateAgo = fmt.Sprintf("%d menit lalu", int(age.Minutes()))
		default:
			data.LastUpdateAgo = fmt.Sprintf("%d jam lalu", int(age.Hours()))
		}
		// Gateway mengunggah berkala; tidak ada pembaruan >3 menit berarti gateway/ESP offline.
		data.DeviceStale = age > 3*time.Minute
	}
	if snapshot.Pump != nil && snapshot.Pump.PumpOn != nil {
		data.PumpOn = *snapshot.Pump.PumpOn
		data.PumpStatusAvailable = true
	} else if latest.PumpOn != nil {
		data.PumpOn = *latest.PumpOn
		data.PumpStatusAvailable = true
	}
	data.ModeLabel = "Tidak tersedia"
	if snapshot.Pump != nil {
		data.ConfigFresh = snapshot.Pump.ConfigActive
		data.ConfigReason = snapshot.Pump.StatusReason
		if snapshot.Pump.AutoOffCM != nil && snapshot.Pump.AutoOnCM != nil &&
			finite(*snapshot.Pump.AutoOffCM) && finite(*snapshot.Pump.AutoOnCM) &&
			*snapshot.Pump.AutoOffCM >= 0 && *snapshot.Pump.AutoOnCM >= 0 {
			data.AutoThresholdAvailable = true
			data.AutoOffDistanceCM = *snapshot.Pump.AutoOffCM
			data.AutoOnDistanceCM = *snapshot.Pump.AutoOnCM
		}
		if snapshot.Pump.Mode != "" {
			data.Mode = normalizeDeviceMode(snapshot.Pump.Mode)
			if data.Mode != "" {
				data.ModeLabel = modeLabel(data.Mode)
			} else {
				data.ModeLabel = snapshot.Pump.Mode
			}
		}
	}
	if data.AutoThresholdAvailable && latest.DistanceCM != nil && latest.WaterHeightCM != nil &&
		finite(*latest.DistanceCM) && finite(*latest.WaterHeightCM) &&
		*latest.DistanceCM >= 0 && *latest.WaterHeightCM >= 0 &&
		latest.SensorConnected && latest.SensorValid && latest.DataFresh {
		referenceHeight := *latest.DistanceCM + *latest.WaterHeightCM
		offWaterHeight := referenceHeight - data.AutoOffDistanceCM
		onWaterHeight := referenceHeight - data.AutoOnDistanceCM
		if finite(offWaterHeight) && finite(onWaterHeight) && offWaterHeight >= 0 && offWaterHeight <= 100 && onWaterHeight >= 0 && onWaterHeight <= 100 {
			data.AutoChartThresholds = true
			data.AutoOffChartY = chartYFromWaterHeight(offWaterHeight)
			data.AutoOnChartY = chartYFromWaterHeight(onWaterHeight)
		}
	}
	data.DeviceWriteAvailable = a.firebase.canWrite()
	if snapshot.Desired != nil {
		data.DesiredUpdatedAtMS = snapshot.Desired.UpdatedAtMS
		data.DesiredStateAvailable = snapshot.Desired.AutoMode != nil && snapshot.Desired.RelayOn != nil
	}
	data.DeviceControlPending = data.DesiredStateAvailable &&
		snapshot.Desired.UpdatedAtMS > 0 &&
		snapshot.Pump != nil &&
		snapshot.Pump.UpdatedAtMS <= snapshot.Desired.UpdatedAtMS
	data.DeviceControlAvailable = data.DeviceWriteAvailable &&
		data.DesiredStateAvailable &&
		snapshot.Desired.UpdatedAtMS > 0 &&
		snapshot.Pump != nil &&
		snapshot.Pump.UpdatedAtMS > snapshot.Desired.UpdatedAtMS
	if historyDirection == "" {
		return
	}
	history, err := a.firebase.getDeviceHistory(ctx, plot.DeviceID, historyDirection, historyCursor)
	if err != nil {
		log.Printf("read Firebase device history %q: %v", plot.DeviceID, err)
		data.DeviceError = "Riwayat perangkat tidak dapat dimuat dari Firebase."
		data.Events = nil
		data.ChartPoints = ""
		return
	}
	data.Events = make([]Activity, 0, len(history.Records))
	for _, record := range history.Records {
		data.Events = append(data.Events, activityFromDeviceEvent(record.Key, record.Event))
	}
	data.ChartPoints = makeDeviceChartPoints(data.Events, time.UnixMilli(data.ChartStartMS))
	if data.Active == "Beranda" && len(data.Events) > 5 {
		data.Events = data.Events[:5]
	}
	data.HistoryPageSize = firebaseHistoryPageSize
	data.HistoryOlderCursor = history.OldestKey
	data.HistoryNewerCursor = history.NewestKey
	data.HasOlderHistory = history.HasOlder
	data.HasNewerHistory = history.HasNewer
	if history.HasOlder {
		data.OlderPage = data.HistoryPage + 1
	}
	if history.HasNewer {
		data.NewerPage = data.HistoryPage - 1
	}
}

func activityFromDeviceEvent(key string, event deviceEvent) Activity {
	when := event.LocalTime
	if when == "" && event.TimestampMS > 0 {
		when = time.UnixMilli(event.TimestampMS).In(time.FixedZone("WITA", 8*60*60)).Format("02/01/2006 15.04.05")
	}
	var details []string
	if event.StatusReason != "" {
		details = append(details, event.StatusReason)
	}
	if event.Fault != "" && event.Fault != event.StatusReason {
		details = append(details, event.Fault)
	}
	if event.SensorStatus != "" && event.SensorStatus != event.Fault {
		details = append(details, "Sensor "+event.SensorStatus)
	}
	if event.WaterHeightCM != nil && finite(*event.WaterHeightCM) {
		details = append(details, fmt.Sprintf("Tinggi air %.1f cm", *event.WaterHeightCM))
	}
	if event.PumpOn != nil {
		if *event.PumpOn {
			details = append(details, "Pompa menyala")
		} else {
			details = append(details, "Pompa mati")
		}
	}
	if event.Trigger != "" {
		details = append(details, event.Trigger)
	}
	activity := Activity{
		Key:         key,
		Time:        when,
		Description: strings.Join(details, " · "),
		Mode:        event.Mode,
		TimestampMS: event.TimestampMS,
	}
	if event.WaterHeightCM != nil && finite(*event.WaterHeightCM) {
		activity.WaterHeightCM = *event.WaterHeightCM
		activity.WaterHeightAvailable = true
	}
	return activity
}

func makeDeviceChartPoints(events []Activity, start time.Time) string {
	const window = 24 * time.Hour
	const width = 720.0
	const height = 140.0
	const top = 10.0
	now := start.Add(window)
	points := make([]Activity, 0, len(events))
	for _, event := range events {
		if event.TimestampMS <= 0 || !event.WaterHeightAvailable {
			continue
		}
		eventTime := time.UnixMilli(event.TimestampMS)
		if eventTime.Before(start) || eventTime.After(now) {
			continue
		}
		points = append(points, event)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].TimestampMS < points[j].TimestampMS })
	if len(points) == 0 {
		return ""
	}
	var result strings.Builder
	for i, event := range points {
		x := float64(time.UnixMilli(event.TimestampMS).Sub(start)) / float64(window) * width
		value := math.Max(0, math.Min(100, event.WaterHeightCM))
		y := top + height*(1-value/100)
		if i > 0 {
			result.WriteByte(' ')
		}
		result.WriteString(strconv.FormatFloat(x, 'f', 1, 64))
		result.WriteByte(',')
		result.WriteString(strconv.FormatFloat(y, 'f', 1, 64))
	}
	if len(points) == 1 {
		result.WriteString(" ")
		event := points[0]
		x := float64(time.UnixMilli(event.TimestampMS).Sub(start))/float64(window)*width + 1
		value := math.Max(0, math.Min(100, event.WaterHeightCM))
		y := top + height*(1-value/100)
		result.WriteString(strconv.FormatFloat(x, 'f', 1, 64))
		result.WriteByte(',')
		result.WriteString(strconv.FormatFloat(y, 'f', 1, 64))
	}
	return result.String()
}

func findPlot(plots []Plot, name string) Plot {
	for _, plot := range plots {
		if plot.Name == name {
			return plot
		}
	}
	return Plot{}
}

func (a *App) selectedDevicePlot(user User, r *http.Request) (Plot, bool, error) {
	plots, err := a.listPlots(user.Email)
	if err != nil {
		return Plot{}, false, err
	}
	selected := r.FormValue("petak")
	if selected == "" {
		selected = r.URL.Query().Get("petak")
	}
	if selected != "" {
		plot := findPlot(plots, selected)
		if plot.Name == "" {
			return Plot{}, false, sql.ErrNoRows
		}
		return plot, plot.DeviceID != "", nil
	}
	if len(plots) == 0 {
		return Plot{}, false, nil
	}
	return plots[0], plots[0].DeviceID != "", nil
}

func (a *App) getUser(email string) (User, error) {
	var user User
	err := a.db.QueryRow(
		"SELECT name, email, phone, password_salt, password_hash FROM users WHERE email = ?",
		email,
	).Scan(&user.Name, &user.Email, &user.Phone, &user.PasswordSalt, &user.PasswordHash)
	return user, err
}

func (a *App) listPlots(email string) ([]Plot, error) {
	rows, err := a.db.Query("SELECT name, device_id FROM petaks WHERE user_email = ? ORDER BY id", email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plots []Plot
	for rows.Next() {
		var plot Plot
		if err := rows.Scan(&plot.Name, &plot.DeviceID); err != nil {
			return nil, err
		}
		if plot.DeviceID == "" {
			plot.DeviceID = a.defaultDeviceID
		}
		plots = append(plots, plot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return plots, nil
}

func selectPetak(data *PageData, name string) bool {
	if name == "" {
		return true
	}
	for _, plot := range data.Petaks {
		if plot.Name == name {
			data.Petak = name
			return true
		}
	}
	return false
}

func (a *App) applyAutomaticControlLocked() {
	shouldPumpOn := a.state.WaterLevel < 80
	if a.state.PumpOn == shouldPumpOn {
		return
	}
	a.state.PumpOn = shouldPumpOn
	if shouldPumpOn {
		a.addEventLocked("Pompa menyala, air di bawah 80 cm", "Otomatis")
		return
	}
	a.addEventLocked("Pompa mati, air mencapai 80 cm", "Otomatis")
}

func (a *App) addEventLocked(description, mode string) {
	a.state.Events = append([]Activity{{
		Time:        time.Now().Format("15.04"),
		Description: description,
		Mode:        mode,
	}}, a.state.Events...)
	if len(a.state.Events) > 30 {
		a.state.Events = a.state.Events[:30]
	}
}

func (a *App) render(w http.ResponseWriter, page string, data PageData) {
	tmpl, ok := a.templates[page]
	if !ok {
		log.Printf("unknown template: %s", page)
		http.Error(w, "Halaman tidak dapat ditampilkan.", http.StatusInternalServerError)
		return
	}
	var body bytes.Buffer
	if err := tmpl.ExecuteTemplate(&body, "layout", data); err != nil {
		log.Printf("render %s template: %v", page, err)
		http.Error(w, "Halaman tidak dapat ditampilkan.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body.Bytes())
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func validEmail(email string) bool {
	address, err := mail.ParseAddress(email)
	return err == nil && address.Address == email
}

func checkPassword(salt, expected []byte, password string) bool {
	if len(salt) == 0 || len(expected) != sha256.Size {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), salt, passwordRounds, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	if iterations < 1 || keyLength < 1 {
		return nil
	}
	blocks := (keyLength + sha256.Size - 1) / sha256.Size
	result := make([]byte, 0, blocks*sha256.Size)
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		var counter [4]byte
		binary.BigEndian.PutUint32(counter[:], uint32(block))
		_, _ = mac.Write(counter[:])
		u := mac.Sum(nil)
		derived := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range derived {
				derived[j] ^= u[j]
			}
		}
		result = append(result, derived...)
	}
	return result[:keyLength]
}

func modeLabel(mode string) string {
	if mode == "manual" {
		return "Manual"
	}
	return "Otomatis"
}

func makeChartPoints(values []float64) string {
	if len(values) < 2 {
		return ""
	}
	const width = 720.0
	const height = 140.0
	const top = 10.0
	var points strings.Builder
	for i, value := range values {
		if value < 0 {
			value = 0
		}
		if value > 100 {
			value = 100
		}
		x := float64(i) * width / float64(len(values)-1)
		y := top + height*(1-value/100)
		if i > 0 {
			points.WriteByte(' ')
		}
		points.WriteString(strconv.FormatFloat(x, 'f', 1, 64))
		points.WriteByte(',')
		points.WriteString(strconv.FormatFloat(y, 'f', 1, 64))
	}
	return points.String()
}

func chartYFromWaterHeight(value float64) float64 {
	const top = 10.0
	const height = 140.0
	value = math.Max(0, math.Min(100, value))
	return top + height*(1-value/100)
}

func chartTimeLabels(now time.Time) []string {
	location := time.FixedZone("WITA", 8*60*60)
	start := now.In(location).Add(-24 * time.Hour)
	labels := make([]string, 0, 5)
	for index := 0; index < 4; index++ {
		labels = append(labels, start.Add(time.Duration(index)*6*time.Hour).Format("15.04"))
	}
	return append(labels, "Sekarang")
}

func initials(name string) string {
	parts := strings.Fields(name)
	if len(parts) == 0 {
		return "U"
	}
	var result strings.Builder
	for i, part := range parts {
		if i == 2 {
			break
		}
		for _, character := range part {
			result.WriteRune(character)
			break
		}
	}
	return strings.ToUpper(result.String())
}

func indonesianDate(now time.Time) string {
	weekdays := [...]string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
	months := [...]string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}
	return fmt.Sprintf("%s, %d %s %d", weekdays[now.Weekday()], now.Day(), months[now.Month()-1], now.Year())
}

func main() {
	if err := loadDotEnv(); err != nil {
		log.Fatal(err)
	}
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}

	address := strings.TrimSpace(os.Getenv("ICARE_ADDR"))
	if address == "" {
		address = "0.0.0.0:18081"
	}
	server := &http.Server{
		Addr:              address,
		Handler:           app,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("iCare berjalan di http://%s", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
