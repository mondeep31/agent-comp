// Package api serves the sandbox HTTP and WebSocket API under /v1.
//
// Every /v1 route requires "Authorization: Bearer <key>"; the key's name owns
// the sandboxes it creates. /healthz is unauthenticated.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/mondeep31/agent-comp/internal/sandbox"
)

const maxJSONBody = 8 << 20

// OwnerHeader carries the authenticated key name to handlers that only see
// request headers (the MCP SDK). The auth middleware always overwrites it, so
// clients cannot spoof it.
const OwnerHeader = "X-Agentcomp-Owner"

// Authenticator maps a bearer token to the key's name.
type Authenticator interface {
	Authenticate(token string) (string, bool)
}

type Options struct {
	Manager *sandbox.Manager
	Auth    Authenticator
	// RequestsPerSecond and Burst rate-limit each key.
	RequestsPerSecond float64
	Burst             int
	Logger            *slog.Logger
	// Extra is mounted under /v1 behind auth, e.g. the MCP endpoint.
	Extra map[string]http.Handler
}

type Server struct {
	opts Options
	m    *sandbox.Manager
	log  *slog.Logger

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
}

func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{
		opts:     opts,
		m:        opts.Manager,
		log:      opts.Logger.With("component", "api"),
		limiters: map[string]*rate.Limiter{},
	}
}

type (
	ownerKey    struct{}
	logOwnerKey struct{}
)

// Owner returns the authenticated key name for a request.
func Owner(ctx context.Context) string {
	s, _ := ctx.Value(ownerKey{}).(string)
	return s
}

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/sandboxes", s.createSandbox)
	v1.HandleFunc("GET /v1/sandboxes", s.listSandboxes)
	v1.HandleFunc("GET /v1/sandboxes/{id}", s.getSandbox)
	v1.HandleFunc("DELETE /v1/sandboxes/{id}", s.deleteSandbox)
	v1.HandleFunc("POST /v1/sandboxes/{id}/pause", s.pauseSandbox)
	v1.HandleFunc("POST /v1/sandboxes/{id}/resume", s.resumeSandbox)
	v1.HandleFunc("POST /v1/sandboxes/{id}/snapshot", s.snapshotSandbox)
	v1.HandleFunc("POST /v1/sandboxes/{id}/exec", s.exec)
	v1.HandleFunc("GET /v1/sandboxes/{id}/exec/ws", s.execWS)
	v1.HandleFunc("GET /v1/sandboxes/{id}/files", s.readFile)
	v1.HandleFunc("PUT /v1/sandboxes/{id}/files", s.writeFile)
	v1.HandleFunc("GET /v1/sandboxes/{id}/ls", s.listDir)
	v1.HandleFunc("GET /v1/sandboxes/{id}/stat", s.stat)
	v1.HandleFunc("GET /v1/snapshots", s.listSnapshots)
	v1.HandleFunc("DELETE /v1/snapshots/{id}", s.deleteSnapshot)
	for pattern, h := range s.opts.Extra {
		v1.Handle(pattern, h)
	}
	v1.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})

	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	root.Handle("/v1/", s.authenticate(v1))
	return s.recoverAndLog(root)
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController and the WebSocket upgrade reach the
// underlying writer (Flush, Hijack).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) recoverAndLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		owner := new(string)
		r = r.WithContext(context.WithValue(r.Context(), logOwnerKey{}, owner))
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
					panic(v)
				}
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeError(sw, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "owner", *owner)
		}()
		next.ServeHTTP(sw, r)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		name, valid := "", false
		if ok {
			name, valid = s.opts.Auth.Authenticate(strings.TrimSpace(token))
		}
		if !valid {
			w.Header().Set("WWW-Authenticate", `Bearer realm="agent-comp"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
			return
		}
		if !s.limiter(name).Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
			return
		}
		if slot, ok := r.Context().Value(logOwnerKey{}).(*string); ok {
			*slot = name
		}
		r.Header.Set(OwnerHeader, name)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerKey{}, name)))
	})
}

func (s *Server) limiter(name string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.limiters[name]
	if l == nil {
		l = rate.NewLimiter(rate.Limit(s.opts.RequestsPerSecond), s.opts.Burst)
		s.limiters[name] = l
	}
	return l
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: msg}})
}

// classify maps a service error to an HTTP status and error code. Unknown
// errors are internal; their text is logged, never returned, because it can
// contain host paths.
func classify(err error) (status int, code string, public bool) {
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		return http.StatusNotFound, "not_found", true
	case errors.Is(err, sandbox.ErrInvalid):
		return http.StatusBadRequest, "invalid_argument", true
	case errors.Is(err, sandbox.ErrQuota):
		return http.StatusTooManyRequests, "quota_exceeded", true
	case errors.Is(err, sandbox.ErrConflict):
		return http.StatusConflict, "conflict", true
	case errors.Is(err, sandbox.ErrPermission):
		return http.StatusForbidden, "permission_denied", true
	case errors.Is(err, sandbox.ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "too_large", true
	case errors.Is(err, sandbox.ErrClosed):
		return http.StatusServiceUnavailable, "unavailable", true
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout", true
	case errors.Is(err, context.Canceled):
		return 499, "canceled", true
	default:
		return http.StatusInternalServerError, "internal", false
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, public := classify(err)
	msg := err.Error()
	if !public {
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "owner", Owner(r.Context()), "err", err)
		msg = "internal error"
	}
	writeError(w, status, code, msg)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: decoding body: %v", sandbox.ErrInvalid, err) //nolint:errorlint // only the sentinel is matched
	}
	return nil
}
