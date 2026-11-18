package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	users     map[string]User
	sessions  map[string]Session
	state     IrrigationState
}

func newApp() (*App, error) {
	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load static assets: %w", err)
	}

	app := &App{
		mux:       http.NewServeMux(),
		templates: make(map[string]*template.Template),
		users:     make(map[string]User),
		sessions:  make(map[string]Session),
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
	app.mux.HandleFunc("POST /pump/mode", app.setPumpMode)
	app.mux.HandleFunc("POST /pump/toggle", app.togglePump)

	return app, nil
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
	a.render(w, "dashboard", a.pageData(user, "Beranda"))
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); ok {
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
	a.mu.Lock()
	user, exists := a.users[email]
	a.mu.Unlock()
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
	if _, ok := a.currentUser(r); ok {
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

	a.mu.Lock()
	if _, exists := a.users[email]; exists {
		a.mu.Unlock()
		a.render(w, "register", PageData{Title: "Buat akun", IsAuth: true, Error: "Email ini sudah terdaftar."})
		return
	}
	a.users[email] = user
	a.mu.Unlock()

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
	a.render(w, "history", a.pageData(user, "Riwayat"))
}

func (a *App) devices(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	a.render(w, "devices", a.pageData(user, "Perangkat"))
}

func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	a.render(w, "profile", a.pageData(user, "Profil"))
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

func (a *App) currentUser(r *http.Request) (User, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return User{}, false
	}

	a.mu.Lock()
	session, exists := a.sessions[cookie.Value]
	if !exists || time.Now().After(session.ExpiresAt) {
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
		return User{}, false
	}
	user, exists := a.users[session.Email]
	a.mu.Unlock()
	if !exists {
		return User{}, false
	}
	return user, true
}

func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	user, ok := a.currentUser(r)
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

func (a *App) pageData(user User, active string) PageData {
	a.mu.Lock()
	state := a.state
	events := append([]Activity(nil), state.Events...)
	a.mu.Unlock()

	name := user.Name
	if name == "" {
		name = "User"
	}
	return PageData{
		Title:       active,
		Active:      active,
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
