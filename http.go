package main

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	metricCommandsRun = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mc_console_bridge_commands_run_total",
		Help: "Console commands accepted and sent to the server.",
	})
	metricCommandsRefused = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "mc_console_bridge_commands_refused_total",
		Help: "Console commands refused by the allowlist.",
	})
)

func init() {
	prometheus.MustRegister(metricCommandsRun, metricCommandsRefused)
}

type server struct {
	cfg     Config
	console *Console
	logger  *slog.Logger
}

func newMux(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /metrics", promhttp.Handler())

	mux.Handle("POST /command", s.authed(s.handleCommand))
	mux.Handle("GET /permissions", s.authed(s.handlePermissions))
	mux.Handle("GET /allowlist", s.authed(s.handleAllowlist))
	mux.Handle("GET /events", s.authed(s.handleEvents))

	return mux
}

func (s *server) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, prefix)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.BridgeToken)) != 1 {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

type commandRequest struct {
	Command string `json:"command"`
}

type commandResponse struct {
	Rule   string `json:"rule"`
	Output string `json:"output"`
}

func (s *server) handleCommand(w http.ResponseWriter, r *http.Request) {
	var req commandRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	rule, err := CheckAllowlist(req.Command)
	if err != nil {
		metricCommandsRefused.Inc()
		s.logger.Info("command refused", "command", req.Command, "error", err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	out, err := s.console.SendCommand(r.Context(), req.Command)
	if err != nil {
		s.logger.Warn("command send failed", "command", req.Command, "error", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	metricCommandsRun.Inc()
	writeJSONResponse(w, commandResponse{Rule: rule, Output: out})
}

func (s *server) handlePermissions(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(s.cfg.DataDir, "permissions.json"))
	if err != nil {
		http.Error(w, "permissions.json unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	perms, err := ParsePermissions(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, perms)
}

func (s *server) handleAllowlist(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(s.cfg.DataDir, "allowlist.json"))
	if err != nil {
		http.Error(w, "allowlist.json unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	entries, err := ParseAllowlist(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONResponse(w, entries)
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	var since int64
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "since must be an integer event ID", http.StatusBadRequest)
			return
		}
		since = parsed
	}
	writeJSONResponse(w, s.console.Events.Since(since))
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
