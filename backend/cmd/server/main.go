package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type app struct {
	mu sync.Mutex
	file string
	origin string
}

type visit struct {
	VisitedAt time.Time `json:"visited_at"`
}

func main() {
	a := &app{
		file: env("LOG_FILE", "./data/visits.jsonl"),
		origin: os.Getenv("ALLOWED_ORIGIN"),
	}
	if err := os.MkdirAll(filepath.Dir(a.file), 0755); err != nil {
		log.Fatalf("create data directory: %v", err)
	}
	if _, err := os.OpenFile(a.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644); err != nil {
		log.Fatalf("open visits file: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("GET /visit", a.recordVisit)
	mux.HandleFunc("GET /logs", a.logs)

	server := &http.Server{
		Addr: env("ADDR", ":8080"),
		Handler: a.cors(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
	log.Printf("Santa counter listening on %s; log file: %s", server.Addr, a.file)
	log.Fatal(server.ListenAndServe())
}

func (a *app) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (a.origin == "" || origin == a.origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
}

func (a *app) recordVisit(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	entry, err := json.Marshal(visit{VisitedAt: time.Now().UTC()})
	if err != nil {
		http.Error(w, "could not encode visit", http.StatusInternalServerError)
		return
	}
	file, err := os.OpenFile(a.file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		log.Printf("open visits file: %v", err)
		http.Error(w, "could not write visit", http.StatusInternalServerError)
		return
	}
	if _, err := file.Write(append(entry, '\n')); err != nil {
		_ = file.Close()
		log.Printf("write visits file: %v", err)
		http.Error(w, "could not write visit", http.StatusInternalServerError)
		return
	}
	if err := file.Close(); err != nil {
		log.Printf("close visits file: %v", err)
		http.Error(w, "could not save visit", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "visits": countLines(a.file)})
}

func (a *app) logs(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	content, err := os.ReadFile(a.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "could not read log file", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(content)
}

func countLines(path string) int {
	content, err := os.ReadFile(path)
	if err != nil || len(content) == 0 {
		return 0
	}
	return strings.Count(string(content), "\n")
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
