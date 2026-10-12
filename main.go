package main

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var idPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)

type App struct {
	store    *Store
	cfg      *Config
	ai       *OpenAI
	tz       *time.Location
	audioDir string
	// One recording at a time: a client retry of an in-flight recording must not process it twice.
	recordingMu sync.Mutex
}

func main() {
	loadDotEnv(".env")
	dataDir := env("DATA_DIR", "data")
	addr := env("ADDR", "127.0.0.1:8000")
	tz, err := time.LoadLocation(env("APP_TZ", "UTC"))
	check(err)
	cfg, err := LoadConfig(dataDir)
	check(err)
	store, err := OpenStore(filepath.Join(dataDir, "app.db"))
	check(err)
	audioDir := filepath.Join(dataDir, "audio")
	check(os.MkdirAll(audioDir, 0o755))

	app := &App{store: store, cfg: cfg, ai: NewOpenAI(os.Getenv("OPENAI_API_KEY")), tz: tz, audioDir: audioDir}
	web, err := fs.Sub(webFS, "web")
	check(err)
	static := http.FileServerFS(web)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", app.getConfig)
	mux.HandleFunc("GET /api/entries", app.getEntries)
	mux.HandleFunc("PUT /api/entries/{id}", app.putEntry)
	mux.HandleFunc("DELETE /api/entries/{id}", app.deleteEntry)
	mux.HandleFunc("POST /api/recordings", app.postRecording)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The service worker owns offline caching; the browser must always revalidate.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))

	slog.Info("listening", "addr", addr, "data", dataDir)
	check(http.ListenAndServe(addr, logRequests(mux)))
}

func (a *App) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"categories":      a.cfg.Categories,
		"payment_methods": a.cfg.PaymentMethods,
		"currencies":      []string{"ARS", "USD"},
	})
}

func (a *App) getEntries(w http.ResponseWriter, r *http.Request) {
	entries, err := a.store.Entries(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (a *App) putEntry(w http.ResponseWriter, r *http.Request) {
	var e Entry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	e.ID = r.PathValue("id")
	if !idPattern.MatchString(e.ID) || e.UpdatedAt == "" || e.Date == "" {
		httpError(w, http.StatusBadRequest, errors.New("id, updated_at and date are required"))
		return
	}
	if e.Status != "review" {
		e.Status = "ok"
	}
	a.sanitize(&e.Currency, &e.Category, &e.PaymentMethod, &e.Amount)
	if err := a.store.PutEntry(r.Context(), e); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	stored, err := a.store.Entry(r.Context(), e.ID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

func (a *App) deleteEntry(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteEntry(r.Context(), r.PathValue("id")); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// postRecording is idempotent on the client-generated id: the phone retries until it gets a 200.
func (a *App) postRecording(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	id := r.FormValue("id")
	recordedAt, err := time.Parse(time.RFC3339, r.FormValue("recorded_at"))
	if !idPattern.MatchString(id) || err != nil {
		httpError(w, http.StatusBadRequest, errors.New("id and recorded_at (RFC 3339) are required"))
		return
	}
	file, header, err := r.FormFile("audio")
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	audio, err := io.ReadAll(file)
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}

	a.recordingMu.Lock()
	defer a.recordingMu.Unlock()
	// Finish even if the phone disconnects mid-request; its retry then finds the result.
	ctx := context.WithoutCancel(r.Context())
	if done, err := a.store.RecordingProcessed(ctx, id); err != nil || done {
		a.writeRecording(w, r, id, err)
		return
	}

	// Safari records audio/mp4; OpenAI picks the decoder from the extension.
	ext := "webm"
	if strings.Contains(header.Header.Get("Content-Type"), "mp4") {
		ext = "m4a"
	}
	audioPath := filepath.Join(a.audioDir, id+"."+ext)
	if err := os.WriteFile(audioPath, audio, 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if err := a.store.SaveRecording(ctx, id, recordedAt.UTC().Format(time.RFC3339), audioPath); err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}

	recordedLocal := recordedAt.In(a.tz)
	transcript, err := a.ai.Transcribe(ctx, audio, "audio."+ext, a.cfg.Terms())
	var parsed []ParsedEntry
	if err == nil && strings.TrimSpace(transcript) != "" {
		parsed, err = a.ai.Parse(ctx, a.cfg, transcript, recordedLocal)
	}
	if err != nil {
		slog.Error("recording failed", "id", id, "err", err)
		_ = a.store.FailRecording(ctx, id, err.Error())
		httpError(w, http.StatusBadGateway, err)
		return
	}

	entries := a.toEntries(id, transcript, recordedLocal, parsed)
	err = a.store.FinishRecording(ctx, id, transcript, entries)
	a.writeRecording(w, r, id, err)
}

// toEntries keeps an unparseable recording as one blank entry holding the transcript, so it is never lost.
func (a *App) toEntries(recordingID, transcript string, recordedAt time.Time, parsed []ParsedEntry) []Entry {
	if len(parsed) == 0 {
		parsed = []ParsedEntry{{Currency: "ARS", Note: transcript}}
	}
	parsed = parsed[:min(len(parsed), 15)]
	entries := make([]Entry, len(parsed))
	for i, p := range parsed {
		date := p.Date
		if _, err := time.Parse("2006-01-02", date); err != nil {
			date = recordedAt.Format("2006-01-02")
		}
		e := Entry{
			ID: fmt.Sprintf("%s-%x", recordingID[:34], i+1), Amount: p.Amount, Currency: p.Currency,
			Category: p.Category, Merchant: p.Merchant, PaymentMethod: p.PaymentMethod, Note: p.Note, Date: date,
		}
		a.sanitize(&e.Currency, &e.Category, &e.PaymentMethod, &e.Amount)
		entries[i] = e
	}
	return entries
}

func (a *App) sanitize(currency *string, category, paymentMethod **string, amount **float64) {
	if *currency != "USD" {
		*currency = "ARS"
	}
	if *category != nil && !slices.Contains(a.cfg.Categories, **category) {
		*category = nil
	}
	if *paymentMethod != nil && !slices.Contains(a.cfg.PaymentMethods, **paymentMethod) {
		*paymentMethod = nil
	}
	if *amount != nil && **amount <= 0 {
		*amount = nil
	}
}

func (a *App) writeRecording(w http.ResponseWriter, r *http.Request, id string, err error) {
	var entries []Entry
	if err == nil {
		entries, err = a.store.RecordingEntries(r.Context(), id)
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "entries": entries})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			slog.Info("request", "method", r.Method, "path", r.URL.Path, "ms", time.Since(start).Milliseconds())
		}
	})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadDotEnv sets KEY=VALUE lines from path without overriding the real environment.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && !strings.HasPrefix(k, "#") && os.Getenv(k) == "" {
			os.Setenv(strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}

func check(err error) {
	if err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}
