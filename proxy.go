package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type maintenancePage struct {
	body []byte
}

type siteRoute struct {
	upstreams           []upstreamEntry
	maintenanceFile     string
	verbose             bool
	compress            bool
	maxRequestBodyBytes int64
	rateLimitRPS        float64
	rateLimitBurst      int
	maxIdleConns        int
	timeouts            TimeoutsConfig
	trustedProxies      []*net.IPNet
	requestHeaders      *HeaderOps
	responseHeaders     *HeaderOps
	cors                *CORSConfig
	redirects           []RedirectRule
	basicAuth           *BasicAuthConfig
	errorPages          map[int][]byte
	hsts                *HSTSConfig
	isTLS               bool
}

type upstreamEntry struct {
	pathPrefix  string
	stripPrefix string
	addPrefix   string
	upstream    string
	methods     map[string]bool // nil = match all
	matchHeader string          // "Name: Value" or empty
}

// statusRecorder wraps http.ResponseWriter to capture status code and bytes written.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// pathRouter dispatches to handlers based on longest-prefix path matching + method/header matchers.
type pathRouter struct {
	routes []pathRouteEntry // sorted by prefix length desc
}

type pathRouteEntry struct {
	prefix      string
	methods     map[string]bool // nil = match all
	matchHeader string
	handler     http.Handler
}

func (e *pathRouteEntry) matches(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, e.prefix) {
		return false
	}
	if e.methods != nil && !e.methods[r.Method] {
		return false
	}
	if e.matchHeader != "" {
		name, value, _ := strings.Cut(e.matchHeader, ":")
		if r.Header.Get(strings.TrimSpace(name)) != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

func (pr *pathRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, route := range pr.routes {
		if route.matches(r) {
			route.handler.ServeHTTP(w, r)
			return
		}
	}
	http.NotFound(w, r)
}

func newSiteRouter(routes map[string]siteRoute, logger *slog.Logger) (http.Handler, []string, error) {
	exactHandlers := make(map[string]http.Handler, len(routes))
	wildcardHandlers := make(map[string]http.Handler) // ".example.com" → handler
	hosts := make([]string, 0, len(routes))

	for host, route := range routes {
		// Per-site transport with site-specific timeouts.
		maxIdle := route.maxIdleConns
		if maxIdle <= 0 {
			maxIdle = 100
		}
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   route.timeouts.Dial.Duration,
				KeepAlive: route.timeouts.KeepAlive.Duration,
			}).DialContext,
			MaxIdleConns:          maxIdle,
			IdleConnTimeout:       route.timeouts.IdleConn.Duration,
			TLSHandshakeTimeout:   route.timeouts.TLSHandshake.Duration,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: route.timeouts.ResponseHeader.Duration,
		}
		maintenance, err := loadMaintenancePage(route.maintenanceFile)
		if err != nil {
			return nil, nil, fmt.Errorf("site %q maintenance page: %w", host, err)
		}

		// Build path-based handlers for this host.
		var entries []pathRouteEntry
		for _, u := range route.upstreams {
			target, err := url.Parse(u.upstream)
			if err != nil {
				return nil, nil, fmt.Errorf("site %q upstream %q: %w", host, u.upstream, err)
			}
			proxy := newReverseProxy(target, transport, maintenance, route.verbose, route.trustedProxies, route.requestHeaders, route.responseHeaders, route.errorPages, u.stripPrefix, u.addPrefix, logger)
			entries = append(entries, pathRouteEntry{
				prefix:      u.pathPrefix,
				methods:     u.methods,
				matchHeader: u.matchHeader,
				handler:     proxy,
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].prefix) > len(entries[j].prefix)
		})

		var hostHandler http.Handler
		if len(entries) == 1 && entries[0].prefix == "/" && entries[0].methods == nil && entries[0].matchHeader == "" {
			hostHandler = entries[0].handler
		} else {
			hostHandler = &pathRouter{routes: entries}
		}

		// Wrap with per-site middleware (innermost to outermost).
		if route.compress {
			hostHandler = compressMiddleware(hostHandler)
		}
		if route.maxRequestBodyBytes > 0 {
			hostHandler = maxBodyMiddleware(hostHandler, route.maxRequestBodyBytes)
		}
		if route.cors != nil {
			hostHandler = corsMiddleware(hostHandler, route.cors)
		}
		if route.basicAuth != nil {
			hostHandler = basicAuthMiddleware(hostHandler, route.basicAuth.Username, route.basicAuth.PasswordHash, route.basicAuth.Realm)
		}
		if len(route.redirects) > 0 {
			hostHandler = redirectMiddleware(hostHandler, route.redirects)
		}
		if route.rateLimitRPS > 0 {
			hostHandler = newRateLimitMiddleware(hostHandler, route.rateLimitRPS, route.rateLimitBurst)
		}

		// HSTS: inject Strict-Transport-Security for TLS sites.
		if route.isTLS && route.hsts != nil && route.hsts.MaxAge > 0 {
			hsts := route.hsts
			hostHandler = hstsMiddleware(hostHandler, hsts)
		}

		normalized := canonicalHost(host)
		// Wildcard hosts: "*.example.com" → stored as suffix ".example.com"
		if strings.HasPrefix(normalized, "*.") {
			suffix := normalized[1:] // ".example.com"
			wildcardHandlers[suffix] = hostHandler
		} else {
			exactHandlers[normalized] = hostHandler
		}
		hosts = append(hosts, normalized)
	}

	// Outer handler: request ID + host dispatch + access log.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		r.Header.Set("X-Request-ID", requestID)
		w.Header().Set("X-Request-ID", requestID)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		h := matchHost(canonicalHost(r.Host), exactHandlers, wildcardHandlers)
		if h == nil {
			http.NotFound(rec, r)
		} else {
			h.ServeHTTP(rec, r)
		}

		logger.Info("access",
			"request_id", requestID,
			"method", r.Method,
			"host", canonicalHost(r.Host),
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"src", r.RemoteAddr,
		)
	}), hosts, nil
}

func newReverseProxy(
	target *url.URL,
	transport http.RoundTripper,
	maintenance maintenancePage,
	verbose bool,
	trustedProxies []*net.IPNet,
	reqHeaders *HeaderOps,
	respHeaders *HeaderOps,
	errorPages map[int][]byte,
	stripPrefix string,
	addPrefix string,
	logger *slog.Logger,
) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director

	proxy.Director = func(req *http.Request) {
		originalDirector(req)

		// Strip forwarding headers from untrusted clients.
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			if !isTrustedProxy(clientIP, trustedProxies) {
				req.Header.Del("X-Forwarded-For")
				req.Header.Del("X-Forwarded-Host")
				req.Header.Del("X-Forwarded-Proto")
				req.Header.Del("X-Real-IP")
			}
		}

		req.Header.Set("X-Forwarded-Host", req.Host)
		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			req.Header.Set("X-Real-IP", clientIP)
		}

		// Path rewriting: strip prefix, then add prefix.
		if stripPrefix != "" {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, stripPrefix)
			if req.URL.Path == "" || req.URL.Path[0] != '/' {
				req.URL.Path = "/" + req.URL.Path
			}
			if req.URL.RawPath != "" {
				req.URL.RawPath = strings.TrimPrefix(req.URL.RawPath, stripPrefix)
				if req.URL.RawPath == "" || req.URL.RawPath[0] != '/' {
					req.URL.RawPath = "/" + req.URL.RawPath
				}
			}
		}
		if addPrefix != "" {
			req.URL.Path = addPrefix + req.URL.Path
			if req.URL.RawPath != "" {
				req.URL.RawPath = addPrefix + req.URL.RawPath
			}
		}

		if reqHeaders != nil {
			applyHeaderOps(req.Header, reqHeaders)
		}

		if verbose {
			logger.Debug("proxy",
				"src", req.RemoteAddr,
				"host", canonicalHost(req.Host),
				"method", req.Method,
				"path", req.URL.Path,
				"target", target.Redacted(),
			)
		}
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		if respHeaders != nil {
			applyHeaderOps(resp.Header, respHeaders)
		}
		// Custom error pages: replace body for matching status codes.
		if body, ok := errorPages[resp.StatusCode]; ok {
			resp.Body.Close()
			resp.Body = newBytesReadCloser(body)
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Type", "text/html; charset=utf-8")
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		}
		return nil
	}

	proxy.Transport = transport
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("upstream unavailable",
			"host", canonicalHost(r.Host),
			"target", target.Redacted(),
			"error", err,
		)
		maintenance.write(w)
	}

	return proxy
}

// bytesReadCloser wraps a byte slice into an io.ReadCloser.
type bytesReadCloser struct {
	data []byte
	pos  int
}

func newBytesReadCloser(b []byte) *bytesReadCloser {
	return &bytesReadCloser{data: b}
}

func (b *bytesReadCloser) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}

func (b *bytesReadCloser) Close() error {
	return nil
}

func isTrustedProxy(ip string, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, network := range trusted {
		if network.Contains(parsed) {
			return true
		}
	}
	return false
}

func applyHeaderOps(h http.Header, ops *HeaderOps) {
	for _, name := range ops.Remove {
		h.Del(name)
	}
	for name, value := range ops.Set {
		h.Set(name, value)
	}
	for name, value := range ops.Add {
		h.Add(name, value)
	}
}

// matchHost looks up a handler by exact match, then falls back to wildcard suffix match.
func matchHost(host string, exact map[string]http.Handler, wildcards map[string]http.Handler) http.Handler {
	if h, ok := exact[host]; ok {
		return h
	}
	// Try wildcard: for "sub.example.com", check ".example.com"
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			suffix := host[i:] // ".example.com"
			if h, ok := wildcards[suffix]; ok {
				return h
			}
		}
	}
	return nil
}

// hstsMiddleware injects Strict-Transport-Security header.
func hstsMiddleware(next http.Handler, cfg *HSTSConfig) http.Handler {
	value := "max-age=" + strconv.Itoa(cfg.MaxAge)
	if cfg.IncludeSubDomains {
		value += "; includeSubDomains"
	}
	if cfg.Preload {
		value += "; preload"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", value)
		next.ServeHTTP(w, r)
	})
}

func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func loadMaintenancePage(path string) (maintenancePage, error) {
	if strings.TrimSpace(path) == "" {
		return maintenancePage{body: []byte(defaultMaintenanceHTML)}, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return maintenancePage{}, err
	}

	if len(strings.TrimSpace(string(body))) == 0 {
		return maintenancePage{}, os.ErrInvalid
	}

	return maintenancePage{body: body}, nil
}

func loadErrorPages(pages map[string]string) (map[int][]byte, error) {
	result := make(map[int][]byte)
	for codeStr, path := range pages {
		code, err := strconv.Atoi(codeStr)
		if err != nil {
			return nil, fmt.Errorf("invalid error page status code %q", codeStr)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("error page %d: %w", code, err)
		}
		result[code] = body
	}
	return result, nil
}

func (m maintenancePage) write(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write(m.body)
}

func canonicalHost(hostport string) string {
	host := strings.TrimSpace(hostport)
	if host == "" {
		return ""
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	} else if strings.Count(host, ":") == 1 {
		if idx := strings.LastIndex(host, ":"); idx > 0 {
			host = host[:idx]
		}
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host
}

const defaultMaintenanceHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Maintenance</title>
  <style>
    body {
      margin: 0;
      min-height: 100vh;
      display: grid;
      place-items: center;
      background: #f6f7f9;
      color: #1f2937;
    }
    .card {
      width: min(90vw, 480px);
      padding: 40px 32px;
      text-align: center;
      background: #fff;
      border: 1px solid #e5e7eb;
    }
    h1 {
      margin: 0 0 12px;
      font-size: 1.8rem;
      font-weight: 600;
    }
    p {
      margin: 0;
      line-height: 1.6;
      color: #6b7280;
    }
  </style>
</head>
<body>
  <main class="card">
    <h1>brb; patching reality</h1>
    <p>short maintenance break</p>
  </main>
</body>
</html>`
