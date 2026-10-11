// Package httpx holds the HTTP plumbing shared by every feature: middleware,
// client IP resolution, CORS / origin enforcement, rate limiting, JSON and
// server-sent-event helpers.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyClientIP
)

// Chain applies middleware so the first argument is the outermost layer.
func Chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}

func RequestID(r *http.Request) string {
	id, _ := r.Context().Value(keyRequestID).(string)
	return id
}

// ClientIP returns the caller's address as resolved by the RealIP middleware.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(keyClientIP).(string); ok {
		return ip
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// RealIP resolves the client address, honouring X-Real-IP / X-Forwarded-For
// only when the direct peer is a trusted proxy (e.g. nginx on localhost).
// Otherwise those headers are attacker-controlled and ignored.
func RealIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := host
			if peer, err := netip.ParseAddr(host); err == nil && isTrusted(peer) {
				if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
					if a, err := netip.ParseAddr(xr); err == nil {
						ip = a.Unmap().String()
					}
				} else if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					// Walk right-to-left, skipping our own proxies.
					parts := strings.Split(xff, ",")
					for i := len(parts) - 1; i >= 0; i-- {
						a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
						if err != nil {
							break
						}
						ip = a.Unmap().String()
						if !isTrusted(a) {
							break
						}
					}
				}
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyClientIP, ip)))
		})
	}
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// statusRecorder captures the status code while staying transparent to
// http.ResponseController (Flush, SetWriteDeadline) and websocket hijacking.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Logger assigns a request id and emits one structured log line per request.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			id := newID()
			w.Header().Set("X-Request-ID", id)
			rec := &statusRecorder{ResponseWriter: w}
			ctx := context.WithValue(r.Context(), keyRequestID, id)
			next.ServeHTTP(rec, r.WithContext(ctx))
			log.LogAttrs(ctx, slog.LevelInfo, "request",
				slog.String("id", id),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.bytes),
				slog.String("ip", ClientIP(r)),
				slog.Duration("dur", time.Since(start)),
			)
		})
	}
}

// Recover turns handler panics into 500s instead of crashing the process.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					log.Error("panic", "err", v, "path", r.URL.Path, "stack", string(debug.Stack()))
					Error(w, http.StatusInternalServerError, "internal", "internal server error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets conservative defaults suitable for a JSON/stream API.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// Origins is the CORS allow-list. Entries are exact origins
// ("https://www.jsonreader.com"); "*" allows any origin (development only).
type Origins struct {
	list    []string
	any     bool
	require bool
}

func NewOrigins(list []string, requireOrigin bool) *Origins {
	o := &Origins{require: requireOrigin}
	for _, s := range list {
		s = strings.TrimRight(strings.TrimSpace(s), "/")
		if s == "*" {
			o.any = true
			continue
		}
		o.list = append(o.list, strings.ToLower(s))
	}
	return o
}

func (o *Origins) Allowed(origin string) bool {
	if origin == "" {
		return false
	}
	return o.any || slices.Contains(o.list, strings.ToLower(origin))
}

// Hosts returns host[:port] patterns for websocket origin verification.
func (o *Origins) Hosts() []string {
	if o.any {
		return []string{"*"}
	}
	var hosts []string
	for _, s := range o.list {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}

// CORS answers preflights and stamps allow headers for permitted origins.
// Cross-site requests from origins outside the allow-list are rejected
// outright (not merely left without CORS headers) so another website cannot
// spend this API's resources through its visitors' browsers.
func (o *Origins) CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		h := w.Header()
		h.Add("Vary", "Origin")
		if origin != "" {
			if !o.Allowed(origin) {
				Error(w, http.StatusForbidden, "origin_not_allowed", "origin not allowed")
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", "X-Request-ID, Content-Disposition, Retry-After")
		} else if o.require && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			Error(w, http.StatusForbidden, "origin_required", "requests must originate from an allowed web origin")
			return
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Content-Type, Accept, Cache-Control, Last-Event-ID")
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
