package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	Time        string
	Description string
	Mode        string
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
	Title         string
	Active        string
	Petak         string
	Petaks        []Plot
	PlotName      string
	DeviceID      string
	IsAuth        bool
	Error         string
	UserName      string
	UserEmail     string
	Initials      string
	Today         string
	WaterLevel    float64
	FlowRate      float64
	FlowPercent   float64
	Pressure      float64
	PressureLabel string
	PumpOn        bool
	Mode          string
	ModeLabel     string
	ChartPoints   string
	Events        []Activity
}

type App struct {
	mux       *http.ServeMux
	templates map[string]*template.Template
	mu        sync.Mutex
	sessions  map[string]Session
	db        *sql.DB
	state     IrrigationState
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
	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load static assets: %w", err)
	}

	app := &App{
		mux:       http.NewServeMux(),
		templates: make(map[string]*template.Template),
		sessions:  make(map[string]Session),
		db:        db,
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
	if _, ok := a.requireUser(w, r); !ok {
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
	if _, ok := a.requireUser(w, r); !ok {
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
	data := PageData{
		Title:       active,
		Active:      active,
		Petaks:      plots,
		UserName:    name,
		UserEmail:   user.Email,
		Initials:    initials(name),
		Today:       indonesianDate(time.Now()),
		WaterLevel:  state.WaterLevel,
		FlowRate:    state.FlowRate,
		FlowPercent: state.FlowRate / 120 * 100,
		Pressure:    state.Pressure,
		PressureLabel: strings.ReplaceAll(
			strconv.FormatFloat(state.Pressure, 'f', 1, 64),
			".",
			",",
		),
		PumpOn:      state.PumpOn,
		Mode:        state.Mode,
		ModeLabel:   modeLabel(state.Mode),
		ChartPoints: makeChartPoints([]float64{43, 58, 74, 64, 87, 93, 81, 100, 96, state.WaterLevel}),
		Events:      events,
	}
	if len(plots) > 0 {
		data.Petak = plots[0].Name
	}
	return data, nil
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
	app, err := newApp()
	if err != nil {
		log.Fatal(err)
	}

	address := strings.TrimSpace(os.Getenv("ICARE_ADDR"))
	if address == "" {
		address = "0.0.0.0:8080"
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
