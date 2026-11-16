// Package httpapi exposes the public redirect endpoint and the management API.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kesha/urlshortener/internal/analytics"
	"github.com/kesha/urlshortener/internal/auth"
	"github.com/kesha/urlshortener/internal/config"
	"github.com/kesha/urlshortener/internal/link"
	"github.com/kesha/urlshortener/internal/ratelimit"
)

// Server wires the routes and their middleware.
type Server struct {
	cfg         *config.Config
	links       *link.Service
	users       auth.Store
	limiter     *ratelimit.Limiter
	clicks      *analytics.Recorder
	ready       func(context.Context) error
	log         *slog.Logger
	visitorSalt string
	trusted     []*net.IPNet
	mux         *http.ServeMux
}

// Options are the Server's dependencies.
type Options struct {
	Config  *config.Config
	Links   *link.Service
	Users   auth.Store
	Limiter *ratelimit.Limiter
	Clicks  *analytics.Recorder
	Ready   func(context.Context) error
	Logger  *slog.Logger
}

// New builds the router. Route patterns use Go's method-and-wildcard syntax, so
// the literal paths win over the "/{code}" catch-all automatically.
func New(o Options) *Server {
	s := &Server{
		cfg:         o.Config,
		links:       o.Links,
		users:       o.Users,
		limiter:     o.Limiter,
		clicks:      o.Clicks,
		ready:       o.Ready,
		log:         o.Logger,
		visitorSalt: o.Config.APIKeyPepper,
		trusted:     o.Config.TrustedProxyCIDRs,
		mux:         http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.readiness)
	s.mux.Handle("POST /api/v1/links", s.api(s.createLink))
	s.mux.Handle("GET /api/v1/links", s.api(s.listLinks))
	s.mux.Handle("GET /api/v1/links/{code}", s.api(s.getLink))
	s.mux.Handle("DELETE /api/v1/links/{code}", s.api(s.deleteLink))
	s.mux.HandleFunc("GET /{code}", s.redirect)

	return s
}

// Handler returns the fully wrapped handler.
func (s *Server) Handler() http.Handler {
	return s.recoverPanic(s.requestID(s.accessLog(s.mux)))
}

// api applies authentication then rate limiting, in that order: the limiter
// budgets per account, not per anonymous caller.
func (s *Server) api(h http.HandlerFunc) http.Handler {
	return s.authenticate(s.rateLimit(h))
}

// --- middleware ---------------------------------------------------------

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "panic", rec, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", w.Header().Get("X-Request-Id"),
			"remote", s.clientIP(r),
		)
	})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := presentedKey(r)
		if key == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing API key")
			return
		}
		user, err := s.users.UserByAPIKeyHash(r.Context(), auth.HashKey(s.cfg.APIKeyPepper, key))
		if errors.Is(err, auth.ErrNotFound) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
			return
		}
		if err != nil {
			s.log.Error("credential lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "internal server error")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, user)))
	})
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := userFrom(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing API key")
			return
		}

		decision, err := s.limiter.Allow(r.Context(), user.ID.String())
		if err != nil {
			// Fail open: a Redis outage must not take the API down with it.
			s.log.Warn("rate limiter unavailable; allowing request", "err", err)
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(decision.Limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(decision.Remaining))
		if !decision.Allowed {
			retry := int(decision.RetryAfter.Seconds() + 0.5)
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many requests; retry later")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- helpers ------------------------------------------------------------

type ctxKey int

const userKey ctxKey = iota

func userFrom(ctx context.Context) (auth.User, bool) {
	u, ok := ctx.Value(userKey).(auth.User)
	return u, ok
}

func presentedKey(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
		return ""
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

// clientIP returns the caller's address. Behind a load balancer the TCP peer
// is the proxy, so X-Forwarded-For is honoured only when that peer is inside a
// configured trusted range. The header is walked right to left, skipping hops
// we control, because the leftmost entry is caller-supplied and spoofable.
func (s *Server) clientIP(r *http.Request) string {
	peer := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		peer = host
	}

	if len(s.trusted) == 0 || !ipInNetworks(peer, s.trusted) {
		return peer
	}

	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		ip := net.ParseIP(candidate)
		if ip == nil {
			return peer
		}
		if ipInNetworks(candidate, s.trusted) {
			continue
		}
		return ip.String()
	}
	return peer
}

func ipInNetworks(raw string, networks []*net.IPNet) bool {
	ip := net.ParseIP(raw)
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
