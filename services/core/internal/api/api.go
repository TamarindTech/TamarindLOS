// Package api exposes the Tool Gateway over REST and MCP (Spec 03 §6).
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/gateway"
)

// Version is the server version reported to clients.
var Version = "0.1.0-dev"

// Server wires HTTP routes to the gateway.
type Server struct {
	G   *gateway.Gateway
	Log *slog.Logger
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /v1/tools", s.authed(s.listTools))
	mux.HandleFunc("POST /v1/tools/{name}", s.authed(s.invokeREST))
	mux.HandleFunc("POST /mcp", s.authed(s.mcp))
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
		// No server-initiated stream in Phase 0.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
	return s.logRequests(mux)
}

type principalKey struct{}

func (s *Server) authed(next func(http.ResponseWriter, *http.Request, *auth.Principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.G.Registry.Authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="tamarind"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"status": "error",
				"error": map[string]string{"code": gateway.ErrUnauthenticated, "message": "missing or invalid bearer token"}})
			return
		}
		next(w, r, p)
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.G.DB.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": Version,
		"policy_pack": s.G.Pack.Name + "@" + s.G.Pack.Version})
}

// listTools returns the catalogue with implementation status.
func (s *Server) listTools(w http.ResponseWriter, _ *http.Request, p *auth.Principal) {
	type item struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Summary     string `json:"summary"`
		RiskTier    string `json:"risk_tier"`
		Autonomy    string `json:"default_autonomy"`
		Implemented bool   `json:"implemented"`
		Permitted   bool   `json:"scopes_granted"`
	}
	var out []item
	for _, n := range s.G.Catalogue.Names {
		t := s.G.Catalogue.Tools[n]
		ok, _ := p.HasScopes(t.Scopes)
		out = append(out, item{n, t.Version, t.Summary, t.RiskTier, t.DefaultAutonomy, s.G.Implemented(n), ok})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

// invokeREST handles POST /v1/tools/{name} with body {"input": {...}, ...envelope}.
func (s *Server) invokeREST(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	var body struct {
		Input map[string]any `json:"input"`
		gateway.Envelope
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"status": "error",
			"error": map[string]string{"code": gateway.ErrValidation, "message": "request body must be JSON: " + err.Error()}})
		return
	}
	if body.TraceID == "" {
		body.TraceID = traceID(r)
	}
	resp := s.G.Invoke(r.Context(), p, r.PathValue("name"), body.Input, body.Envelope)
	writeJSON(w, resp.HTTPStatus(), resp)
}

func traceID(r *http.Request) string {
	// W3C traceparent: version-traceid-parentid-flags
	if parts := strings.Split(r.Header.Get("traceparent"), "-"); len(parts) == 4 && len(parts[1]) == 32 {
		return parts[1]
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}
