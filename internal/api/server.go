// Package api serves the sandbox HTTP and WebSocket API under /v1.
//
// Every /v1 route requires "Authorization: Bearer <key>"; the key's name owns
// the sandboxes it creates. /healthz is unauthenticated.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/mondeep31/agent-comp/internal/metrics"
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
	// Metrics is optional.
	Metrics *metrics.Metrics
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

type ownerKey struct{}

// Owner returns the authenticated key name for a request.
func Owner(ctx context.Context) string {
	s, _ := ctx.Value(ownerKey{}).(string)
	return s
}

// reqInfo is filled in by inner handlers and read by the outer logging
// middleware, which only sees its own copy of the request.
type reqInfo struct {
	owner string
	route string
}

type reqInfoKey struct{}

func info(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

// Handler returns the full HTTP handler.
func (s *Server) Handler() http.Handler {
	v1 := http.NewServeMux()
	route := func(mux *http.ServeMux, pattern string, h http.Handler) {
		mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			info(r).route = pattern
			h.ServeHTTP(w, r)
		}))
	}
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /v1/sandboxes":               s.createSandbox,
		"GET /v1/sandboxes":                s.listSandboxes,
		"GET /v1/sandboxes/{id}":           s.getSandbox,
		"DELETE /v1/sandboxes/{id}":        s.deleteSandbox,
		"POST /v1/sandboxes/{id}/pause":    s.pauseSandbox,
		"POST /v1/sandboxes/{id}/resume":   s.resumeSandbox,
		"POST /v1/sandboxes/{id}/snapshot": s.snapshotSandbox,
		"POST /v1/sandboxes/{id}/exec":     s.exec,
		"GET /v1/sandboxes/{id}/exec/ws":   s.execWS,
		"GET /v1/sandboxes/{id}/files":     s.readFile,
		"PUT /v1/sandboxes/{id}/files":     s.writeFile,
		"GET /v1/sandboxes/{id}/ls":        s.listDir,
		"GET /v1/sandboxes/{id}/stat":      s.stat,
		"GET /v1/snapshots":                s.listSnapshots,
		"DELETE /v1/snapshots/{id}":        s.deleteSnapshot,
	} {
		route(v1, pattern, h)
	}
	for pattern, h := range s.opts.Extra {
		route(v1, pattern, h)
	}
	v1.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such route")
	})

	root := http.NewServeMux()
	route(root, "GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))
	root.Handle("/v1/", s.authenticate(v1))
	return s.recoverAndLog(root)
}

// requestID reuses Cloudflare's ray id or a sane client id, else makes one.
func requestID(r *http.Request) string {
	for _, h := range []string{"X-Request-Id", "Cf-Ray"} {
		if v := r.Header.Get(h); v != "" && len(v) <= 64 && strings.Trim(v, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") == "" {
			return v
		}
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
		ri := &reqInfo{route: "unmatched"}
		id := requestID(r)
		w.Header().Set("X-Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri))
		defer func() {
			v := recover()
			if v != nil && v != http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does
				s.log.Error("panic serving request", "request_id", id, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeError(sw, http.StatusInternalServerError, "internal", "internal error")
				}
			}
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			elapsed := time.Since(start)
			s.log.Info("request", "request_id", id, "method", r.Method, "path", r.URL.Path, "route", ri.route,
				"status", sw.status, "bytes", sw.bytes, "ms", elapsed.Milliseconds(), "owner", ri.owner)
			if s.opts.Metrics != nil {
				s.opts.Metrics.ObserveRequest(ri.route, r.Method, sw.status, elapsed)
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // as above
				panic(v)
			}
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
		info(r).owner = name
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
