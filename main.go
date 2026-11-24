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

var errFirebasePlotExists = errors.New("Firebase plot already exists")

type User struct {
	Name         string
	Email        string
	Phone        string
	PasswordSalt []byte
	PasswordHash []byte
	Role         string
	FirebaseUID  string
}

type AdminUser struct {
	UID   string
	Email string
	Name  string
	Phone string
	Role  string
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

type HistoryPageLink struct {
	Number int
	URL    string
}

type Plot struct {
	Name      string
	DeviceID  string
	OwnerUID  string
	OwnerName string
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
	PetakOwnerUID          string
	PetakOwnerName         string
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
	HistoryPageLinks       []HistoryPageLink
	HistoryPageSize        int
	HistoryOlderCursor     string
	HasOlderHistory        bool
	IsSuperAdmin           bool
	AdminUsers             []AdminUser
}

type App struct {
	mux           *http.ServeMux
	templates     map[string]*template.Template
	mu            sync.Mutex
	sessions      map[string]Session
	googleOAuth   *googleOAuthConfig
	googleStates  map[string]googleOAuthState
	db            *sql.DB
	state         IrrigationState
	firebase      *firebaseRTDB
	userFirebase  *firebaseRTDB
	pumpCommandMu sync.Mutex
	// defaultDeviceID dipakai untuk petak yang belum punya ID perangkat sendiri
	// (ICARE_DEFAULT_DEVICE_ID), sehingga dashboard tidak jatuh ke data demo.
	defaultDeviceID string
	superAdminEmail string
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
	superAdminEmail := normalizeEmail(os.Getenv("ICARE_SUPER_ADMIN_EMAIL"))
	if superAdminEmail == "" {
		superAdminEmail = "web@icare.local"
	}
	if firebase != nil && firebase.email != "" && normalizeEmail(firebase.email) != superAdminEmail {
		return nil, errors.New("ICARE_FIREBASE_EMAIL must match ICARE_SUPER_ADMIN_EMAIL to access user profiles")
	}

	app := &App{
		mux:             http.NewServeMux(),
		templates:       make(map[string]*template.Template),
		sessions:        make(map[string]Session),
		googleStates:    make(map[string]googleOAuthState),
		db:              db,
		firebase:        firebase,
		userFirebase:    firebase,
		defaultDeviceID: strings.TrimSpace(os.Getenv("ICARE_DEFAULT_DEVICE_ID")),
		superAdminEmail: superAdminEmail,
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
	app.mux.HandleFunc("POST /admin/plots", app.adminAddPlot)
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
	role TEXT NOT NULL DEFAULT 'user',
	firebase_uid TEXT NOT NULL DEFAULT '',
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
	for _, column := range []struct {
		name string
		sql  string
	}{
		{name: "role", sql: "ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'user'"},
		{name: "firebase_uid", sql: "ALTER TABLE users ADD COLUMN firebase_uid TEXT NOT NULL DEFAULT ''"},
	} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = ?", column.name).Scan(&count); err != nil {
			return fmt.Errorf("check users column %s: %w", column.name, err)
		}
		if count == 0 {
			if _, err := db.Exec(column.sql); err != nil {
				return fmt.Errorf("migrate users column %s: %w", column.name, err)
			}
		}
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
	data, err := a.pageData(r.Context(), user, "Beranda")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	if !selectPetak(&data, r.URL.Query().Get("petak"), r.URL.Query().Get("owner_uid")) {
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
	if a.firebase == nil || a.firebase.apiKey == "" || a.userFirebase == nil {
		a.render(w, "login", PageData{Title: "Masuk", IsAuth: true, Error: "Konfigurasi Firebase Auth belum tersedia."})
		return
	}
	auth, err := a.firebase.signInUserWithPassword(r.Context(), email, password)
	if err != nil {
		a.render(w, "login", PageData{
			Title:  "Masuk",
			IsAuth: true,
			Error:  "Email atau kata sandi tidak sesuai.",
		})
		return
	}

	profile, found, err := a.userFirebase.getUserProfile(r.Context(), auth.LocalID)
	if err != nil {
		log.Printf("read Firebase user profile failed: %v", err)
		http.Error(w, "Profil Firebase tidak dapat dibaca. Periksa rules Realtime Database.", http.StatusBadGateway)
		return
	}
	if !found {
		profile = firebaseUserProfile{
			UID:    auth.LocalID,
			Name:   strings.SplitN(auth.Email, "@", 2)[0],
			Email:  auth.Email,
			Role:   "user",
			Petaks: make(map[string]firebaseUserPlot),
		}
	}
	needsProfileWrite := !found
	if profile.Email != auth.Email {
		profile.Email = auth.Email
		needsProfileWrite = true
	}
	if profile.Name == "" {
		profile.Name = strings.SplitN(auth.Email, "@", 2)[0]
		needsProfileWrite = true
	}
	role := "user"
	if auth.Email == a.superAdminEmail {
		role = "super_admin"
	}
	if profile.Role != role {
		profile.Role = role
		needsProfileWrite = true
	}
	if profile.Petaks == nil {
		profile.Petaks = make(map[string]firebaseUserPlot)
		needsProfileWrite = true
	}
	if needsProfileWrite {
		if err := a.userFirebase.putUserProfile(r.Context(), profile); err != nil {
			log.Printf("write Firebase user profile failed: %v", err)
			http.Error(w, "Profil akun tidak dapat disimpan ke Firebase Realtime Database.", http.StatusBadGateway)
			return
		}
	}
	user, err := a.saveUserMirror(profile)
	if err != nil {
		http.Error(w, "Profil akun tidak dapat disiapkan.", http.StatusInternalServerError)
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
	case email == a.superAdminEmail:
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Akun super admin hanya dapat dibuat dan dikelola di Firebase Authentication."})
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

	if a.firebase == nil || a.firebase.apiKey == "" || a.userFirebase == nil {
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Konfigurasi Firebase Auth belum tersedia."})
		return
	}
	auth, err := a.firebase.signUpWithPassword(r.Context(), email, password)
	if err != nil {
		message := "Akun Firebase tidak dapat dibuat."
		if strings.Contains(err.Error(), "EMAIL_EXISTS") {
			message = "Email ini sudah terdaftar di Firebase."
		}
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: message})
		return
	}

	profile := firebaseUserProfile{
		UID:    auth.LocalID,
		Name:   name,
		Email:  auth.Email,
		Phone:  phone,
		Role:   "user",
		Petaks: make(map[string]firebaseUserPlot),
	}
	if err := a.userFirebase.putUserProfile(r.Context(), profile); err != nil {
		log.Printf("write Firebase registration profile failed: %v", err)
		http.Error(w, "Akun Firebase dibuat, tetapi profil tidak dapat disimpan ke Realtime Database. Periksa rules Firebase.", http.StatusBadGateway)
		return
	}
	if _, err := a.saveUserMirror(profile); err != nil {
		http.Error(w, "Akun tersimpan di Firebase tetapi profil lokal tidak dapat diperbarui.", http.StatusInternalServerError)
		return
	}

	if err := a.startSession(w, r, auth.Email); err != nil {
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
	data, err := a.pageData(r.Context(), user, "Riwayat")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	if !selectPetak(&data, r.URL.Query().Get("petak"), r.URL.Query().Get("owner_uid")) {
		http.NotFound(w, r)
		return
	}
	before := r.URL.Query().Get("before")
	after := r.URL.Query().Get("after")
	cursors := r.URL.Query()["cursor"]
	if before != "" && after != "" {
		http.Error(w, "Gunakan satu cursor riwayat saja.", http.StatusBadRequest)
		return
	}
	if (before != "" && !validFirebaseKey(before)) || (after != "" && !validFirebaseKey(after)) {
		http.Error(w, "Cursor riwayat tidak valid.", http.StatusBadRequest)
		return
	}
	for _, cursor := range cursors {
		if !validFirebaseKey(cursor) {
			http.Error(w, "Cursor riwayat tidak valid.", http.StatusBadRequest)
			return
		}
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
	} else if page > 1 {
		if len(cursors) < page-1 {
			http.Error(w, "Cursor halaman riwayat tidak tersedia. Kembali ke halaman 1.", http.StatusBadRequest)
			return
		}
		direction, cursor = "before", cursors[page-2]
	}
	data.HistoryPage = page
	data.HistoryPageSize = firebaseHistoryPageSize
	a.loadDeviceData(r.Context(), &data, direction, cursor)
	if page == 2 && before != "" && len(cursors) == 0 {
		cursors = append(cursors, before)
	}
	if data.HasOlderHistory && len(cursors) == page-1 && data.HistoryOlderCursor != "" {
		cursors = append(cursors, data.HistoryOlderCursor)
	}
	data.HistoryPageLinks = buildHistoryPageLinks(data.Petak, data.PetakOwnerUID, cursors)
	a.render(w, "history", data)
}

func buildHistoryPageLinks(petak, ownerUID string, cursors []string) []HistoryPageLink {
	links := make([]HistoryPageLink, 0, len(cursors)+1)
	for page := 1; page <= len(cursors)+1; page++ {
		query := url.Values{
			"page":  {strconv.Itoa(page)},
			"petak": {petak},
		}
		if ownerUID != "" {
			query.Set("owner_uid", ownerUID)
		}
		for _, cursor := range cursors {
			query.Add("cursor", cursor)
		}
		links = append(links, HistoryPageLink{
			Number: page,
			URL:    "/history?" + query.Encode(),
		})
	}
	return links
}

func (a *App) devices(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !a.isSuperAdmin(user) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data, err := a.pageData(r.Context(), user, "Perangkat")
	if err != nil {
		http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
		return
	}
	if !selectPetak(&data, r.URL.Query().Get("petak"), r.URL.Query().Get("owner_uid")) {
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
	if !a.isSuperAdmin(user) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data, err := a.pageData(r.Context(), user, "Manajemen")
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
	if !a.isSuperAdmin(user) {
		http.Error(w, "Hanya super admin yang dapat menambah atau mengubah petak.", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	deviceID := strings.TrimSpace(r.FormValue("device_id"))
	if name == "" || !utf8.ValidString(name) || len(name) > 50 || !validFirebaseKey(name) ||
		len(deviceID) > 128 || (deviceID != "" && !validFirebaseKey(deviceID)) {
		data, err := a.pageData(r.Context(), user, "Beranda")
		if err != nil {
			http.Error(w, "Data petak tidak dapat dimuat.", http.StatusInternalServerError)
			return
		}
		data.Error = "Nama petak wajib diisi (maksimal 50 karakter); ID perangkat maksimal 128 karakter."
		data.PlotName = name
		data.DeviceID = deviceID
		if selected := r.FormValue("petak"); selected != "" {
			_ = selectPetak(&data, selected, r.FormValue("owner_uid"))
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		a.render(w, "dashboard", data)
		return
	}
	err := a.addFirebasePlot(r.Context(), user.FirebaseUID, name, deviceID)
	if errors.Is(err, errFirebasePlotExists) {
		data, err := a.pageData(r.Context(), user, "Beranda")
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
	if err != nil {
		log.Printf("add Firebase plot for %q: %v", user.FirebaseUID, err)
		http.Error(w, "Petak tidak dapat disimpan ke Firebase Realtime Database.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/?petak="+url.QueryEscape(name), http.StatusSeeOther)
}

func (a *App) adminAddPlot(w http.ResponseWriter, r *http.Request) {
	admin, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if !a.isSuperAdmin(admin) {
		http.Error(w, "Hanya super admin yang dapat menambah petak.", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Form tidak dapat dibaca.", http.StatusBadRequest)
		return
	}
	uid := strings.TrimSpace(r.FormValue("user_uid"))
	name := strings.TrimSpace(r.FormValue("name"))
	deviceID := strings.TrimSpace(r.FormValue("device_id"))
	if !validFirebaseKey(uid) || name == "" || !utf8.ValidString(name) || len(name) > 50 ||
		!validFirebaseKey(name) || len(deviceID) > 128 || (deviceID != "" && !validFirebaseKey(deviceID)) {
		http.Error(w, "Data petak atau pengguna tidak valid.", http.StatusUnprocessableEntity)
		return
	}
	err := a.addFirebasePlot(r.Context(), uid, name, deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, errFirebasePlotExists) {
		http.Redirect(w, r, "/profile?error=Petak+sudah+ada", http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("admin add Firebase plot for %q: %v", uid, err)
		http.Error(w, "Petak tidak dapat disimpan ke Firebase Realtime Database.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
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

	http.Error(w, "Petak belum ditautkan ke perangkat Firebase; kontrol simulasi lokal dinonaktifkan.", http.StatusConflict)
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
	a.pumpCommandError(w, r, http.StatusConflict, "Petak belum ditautkan ke perangkat Firebase; kontrol simulasi lokal dinonaktifkan.")
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
		if ownerUID := r.FormValue("owner_uid"); ownerUID != "" {
			location += "&owner_uid=" + url.QueryEscape(ownerUID)
		}
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

func (a *App) pageData(ctx context.Context, user User, active string) (PageData, error) {
	if a.userFirebase == nil {
		return PageData{}, errors.New("Firebase Realtime Database is not configured")
	}
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	profile, found, err := a.userFirebase.getUserProfile(ctx, user.FirebaseUID)
	if err != nil {
		return PageData{}, err
	}
	if !found {
		return PageData{}, errors.New("Firebase user profile is missing")
	}
	plots := plotsFromFirebaseProfile(profile, a.defaultDeviceID)
	isSuperAdmin := a.isSuperAdmin(user)
	if isSuperAdmin {
		profiles, err := a.userFirebase.listUserProfiles(ctx)
		if err != nil {
			return PageData{}, err
		}
		plots = append([]Plot(nil), plotsFromFirebaseProfile(profile, a.defaultDeviceID)...)
		for _, otherProfile := range profiles {
			if otherProfile.UID == profile.UID {
				continue
			}
			plots = append(plots, plotsFromFirebaseProfile(otherProfile, a.defaultDeviceID)...)
		}
	}

	name := profile.Name
	if name == "" {
		name = user.Name
	}
	if name == "" {
		name = "User"
	}
	chartNow := time.Now()
	data := PageData{
		Title:        active,
		Active:       active,
		Petaks:       plots,
		UserName:     name,
		UserEmail:    profile.Email,
		IsSuperAdmin: isSuperAdmin,
		Initials:     initials(name),
		Today:        indonesianDate(chartNow),
		WaterLevel:   state.WaterLevel,
		FlowRate:     state.FlowRate,
		FlowPercent:  state.FlowRate / 120 * 100,
		Pressure:     state.Pressure,
		PressureLabel: strings.ReplaceAll(
			strconv.FormatFloat(state.Pressure, 'f', 1, 64),
			".",
			",",
		),
		PumpOn:       state.PumpOn,
		Mode:         state.Mode,
		ModeLabel:    modeLabel(state.Mode),
		ChartPoints:  "",
		ChartStartMS: chartNow.Add(-24 * time.Hour).UnixMilli(),
		ChartLabels:  chartTimeLabels(chartNow),
		Events:       nil,
	}
	if len(plots) > 0 {
		data.Petak = plots[0].Name
		data.PetakOwnerUID = plots[0].OwnerUID
		data.PetakOwnerName = plots[0].OwnerName
	}
	if data.IsSuperAdmin {
		adminUsers, err := a.listUsers(ctx)
		if err != nil {
			return PageData{}, err
		}
		data.AdminUsers = adminUsers
	}
	return data, nil
}

func (a *App) loadDeviceData(ctx context.Context, data *PageData, historyDirection, historyCursor string) {
	plot := findPlot(data.Petaks, data.Petak, data.PetakOwnerUID)
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
		data.FlowPercent = math.Min(100, data.DebitLPM/120*100)
	} else {
		data.FlowPercent = 0
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
	data.HasOlderHistory = history.HasOlder
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

func findPlot(plots []Plot, name, ownerUID string) Plot {
	for _, plot := range plots {
		if plot.Name == name && (ownerUID == "" || plot.OwnerUID == ownerUID) {
			return plot
		}
	}
	return Plot{}
}

func (a *App) selectedDevicePlot(user User, r *http.Request) (Plot, bool, error) {
	if a.userFirebase == nil {
		return Plot{}, false, errors.New("Firebase Realtime Database is not configured")
	}
	ownerUID := r.FormValue("owner_uid")
	if ownerUID == "" {
		ownerUID = r.URL.Query().Get("owner_uid")
	}
	profileUID := user.FirebaseUID
	if ownerUID != "" && ownerUID != user.FirebaseUID {
		if !a.isSuperAdmin(user) {
			return Plot{}, false, sql.ErrNoRows
		}
		profileUID = ownerUID
	}
	profile, found, err := a.userFirebase.getUserProfile(r.Context(), profileUID)
	if err != nil {
		return Plot{}, false, err
	}
	if !found {
		return Plot{}, false, sql.ErrNoRows
	}
	plots := plotsFromFirebaseProfile(profile, a.defaultDeviceID)
	selected := r.FormValue("petak")
	if selected == "" {
		selected = r.URL.Query().Get("petak")
	}
	if selected != "" {
		plot := findPlot(plots, selected, profileUID)
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
		"SELECT name, email, phone, password_salt, password_hash, role, firebase_uid FROM users WHERE email = ?",
		email,
	).Scan(&user.Name, &user.Email, &user.Phone, &user.PasswordSalt, &user.PasswordHash, &user.Role, &user.FirebaseUID)
	return user, err
}

func (a *App) saveUserMirror(profile firebaseUserProfile) (User, error) {
	_, err := a.db.Exec(
		"INSERT INTO users (email, name, phone, password_salt, password_hash, role, firebase_uid) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(email) DO UPDATE SET role = excluded.role, firebase_uid = excluded.firebase_uid",
		profile.Email, profile.Name, profile.Phone, []byte{}, []byte{}, profile.Role, profile.UID,
	)
	if err != nil {
		return User{}, fmt.Errorf("save Firebase user mirror: %w", err)
	}
	return a.getUser(profile.Email)
}

func (a *App) isSuperAdmin(user User) bool {
	return normalizeEmail(user.Email) == a.superAdminEmail
}

func (a *App) listUsers(ctx context.Context) ([]AdminUser, error) {
	if a.userFirebase == nil {
		return nil, errors.New("Firebase Realtime Database is not configured")
	}
	profiles, err := a.userFirebase.listUserProfiles(ctx)
	if err != nil {
		return nil, err
	}
	users := make([]AdminUser, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Email == a.superAdminEmail || profile.Role == "super_admin" {
			continue
		}
		users = append(users, AdminUser{
			UID:   profile.UID,
			Email: profile.Email,
			Name:  profile.Name,
			Phone: profile.Phone,
			Role:  "user",
		})
	}
	return users, nil
}

func (a *App) addFirebasePlot(ctx context.Context, uid, name, deviceID string) error {
	if a.userFirebase == nil {
		return errors.New("Firebase Realtime Database is not configured")
	}
	profile, found, err := a.userFirebase.getUserProfile(ctx, uid)
	if err != nil {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	if _, exists := profile.Petaks[name]; exists {
		return errFirebasePlotExists
	}
	return a.userFirebase.putUserPlot(ctx, uid, name, deviceID)
}

func plotsFromFirebaseProfile(profile firebaseUserProfile, defaultDeviceID string) []Plot {
	names := make([]string, 0, len(profile.Petaks))
	for name := range profile.Petaks {
		names = append(names, name)
	}
	sort.Strings(names)
	plots := make([]Plot, 0, len(names))
	ownerName := profile.Name
	if ownerName == "" {
		ownerName = profile.Email
	}
	for _, name := range names {
		deviceID := profile.Petaks[name].DeviceID
		if deviceID == "" {
			deviceID = defaultDeviceID
		}
		plots = append(plots, Plot{
			Name:      name,
			DeviceID:  deviceID,
			OwnerUID:  profile.UID,
			OwnerName: ownerName,
		})
	}
	return plots
}

func selectPetak(data *PageData, name, ownerUID string) bool {
	if name == "" {
		return true
	}
	for _, plot := range data.Petaks {
		if plot.Name == name && (ownerUID == "" || plot.OwnerUID == ownerUID) {
			data.Petak = name
			data.PetakOwnerUID = plot.OwnerUID
			data.PetakOwnerName = plot.OwnerName
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
