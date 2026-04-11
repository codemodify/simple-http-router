package main

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/time/rate"
)

// --- gzip compression middleware ---

func compressMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz := &gzipResponseWriter{ResponseWriter: w}
		defer gz.finish()

		w.Header().Set("Vary", "Accept-Encoding")
		next.ServeHTTP(gz, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gzWriter    *gzip.Writer
	decided     bool
	compressing bool
}

func (g *gzipResponseWriter) decide() {
	g.decided = true
	h := g.ResponseWriter.Header()
	if h.Get("Content-Encoding") != "" {
		return
	}
	if isCompressible(h.Get("Content-Type")) {
		g.compressing = true
		g.gzWriter = gzip.NewWriter(g.ResponseWriter)
	}
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if !g.decided {
		g.decide()
	}
	if g.compressing {
		g.ResponseWriter.Header().Del("Content-Length")
		g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if g.ResponseWriter.Header().Get("Content-Type") == "" {
			g.ResponseWriter.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.decide()
		if g.compressing {
			g.ResponseWriter.Header().Del("Content-Length")
			g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
		}
	}
	if g.compressing {
		return g.gzWriter.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	if g.gzWriter != nil {
		_ = g.gzWriter.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipResponseWriter) finish() {
	if g.gzWriter != nil {
		_ = g.gzWriter.Close()
	}
}

func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}

func isCompressible(contentType string) bool {
	ct := strings.ToLower(contentType)
	// Strip parameters (e.g. "; charset=utf-8")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)

	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json",
		"application/javascript",
		"application/xml",
		"application/xhtml+xml",
		"application/wasm",
		"application/manifest+json",
		"application/vnd.api+json",
		"application/atom+xml",
		"application/rss+xml",
		"image/svg+xml":
		return true
	}
	return false
}

// --- rate limiting middleware ---

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*limiterEntry
	limit    rate.Limit
	burst    int
	checks   int
}

func (rl *ipRateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Periodic cleanup: every 1000 checks, evict entries idle > 3 min.
	rl.checks++
	if rl.checks >= 1000 {
		rl.checks = 0
		cutoff := time.Now().Add(-3 * time.Minute)
		for k, v := range rl.limiters {
			if v.lastSeen.Before(cutoff) {
				delete(rl.limiters, k)
			}
		}
	}

	entry, ok := rl.limiters[ip]
	if !ok {
		entry = &limiterEntry{limiter: rate.NewLimiter(rl.limit, rl.burst)}
		rl.limiters[ip] = entry
	}
	entry.lastSeen = time.Now()
	return entry.limiter.Allow()
}

func newRateLimitMiddleware(next http.Handler, rps float64, burst int) http.Handler {
	if burst <= 0 {
		burst = max(int(rps), 1)
	}
	rl := &ipRateLimiter{
		limiters: make(map[string]*limiterEntry),
		limit:    rate.Limit(rps),
		burst:    burst,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := strings.Cut(r.RemoteAddr, ":")
		// Handle IPv6 [::1]:port format
		if strings.HasPrefix(r.RemoteAddr, "[") {
			if i := strings.LastIndex(r.RemoteAddr, "]"); i >= 0 {
				ip = r.RemoteAddr[1:i]
			}
		}
		if !rl.allow(ip) {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- request body size limit middleware ---

func maxBodyMiddleware(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		next.ServeHTTP(w, r)
	})
}

// --- CORS middleware ---

func corsMiddleware(next http.Handler, cfg *CORSConfig) http.Handler {
	allowedOrigins := make(map[string]bool)
	allowAll := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
		}
		allowedOrigins[o] = true
	}
	methods := "GET, POST, PUT, DELETE, OPTIONS, PATCH, HEAD"
	if len(cfg.AllowedMethods) > 0 {
		methods = strings.Join(cfg.AllowedMethods, ", ")
	}
	headers := "Content-Type, Authorization, X-Request-ID"
	if len(cfg.AllowedHeaders) > 0 {
		headers = strings.Join(cfg.AllowedHeaders, ", ")
	}
	maxAge := "86400"
	if cfg.MaxAge > 0 {
		maxAge = strconv.Itoa(cfg.MaxAge)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && (allowAll || allowedOrigins[origin]) {
			allow := origin
			if allowAll {
				allow = "*"
			}
			w.Header().Set("Access-Control-Allow-Origin", allow)
			w.Header().Set("Access-Control-Allow-Methods", methods)
			w.Header().Set("Access-Control-Allow-Headers", headers)
			w.Header().Set("Access-Control-Max-Age", maxAge)
			if !allowAll {
				w.Header().Set("Vary", "Origin")
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- basic auth middleware ---

func basicAuthMiddleware(next http.Handler, username, passwordHash, realm string) http.Handler {
	if realm == "" {
		realm = "Restricted"
	}
	challenge := `Basic realm="` + realm + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != username || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(pass)) != nil {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- redirect middleware ---

func redirectMiddleware(next http.Handler, rules []RedirectRule) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rule := range rules {
			if strings.HasPrefix(r.URL.Path, rule.From) {
				target := rule.To + strings.TrimPrefix(r.URL.Path, rule.From)
				status := rule.Status
				if status == 0 {
					status = http.StatusMovedPermanently
				}
				http.Redirect(w, r, target, status)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// --- connection limit middleware ---

func maxConnectionsMiddleware(next http.Handler, maxConns int) http.Handler {
	sem := make(chan struct{}, maxConns)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		}
	})
}
