package main

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testRoute(upstream string) siteRoute {
	return siteRoute{
		upstreams: []upstreamEntry{{pathPrefix: "/", upstream: upstream}},
		timeouts:  TimeoutsConfig{}.withDefaults(),
	}
}

func TestSiteRouterProxiesHealthyUpstream(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "ok")
		_, _ = io.WriteString(w, "proxied")
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute(upstream.URL),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/hello", nil)
	req.Host = "app.example.com:443"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if string(body) != "proxied" {
		t.Fatalf("expected proxied body, got %q", string(body))
	}
}

func TestSiteRouterReturnsMaintenanceWhenUpstreamIsDown(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute("http://" + addr),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if !strings.Contains(string(body), "Maintenance") {
		t.Fatalf("expected maintenance page, got %q", string(body))
	}
}

func TestSiteRouterReturnsNotFoundForUnknownHost(t *testing.T) {
	t.Parallel()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute("http://127.0.0.1:8080"),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://unknown.example.com/", nil)
	req.Host = "unknown.example.com"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, recorder.Code)
	}
}

func TestLoadMaintenancePageRejectsEmptyFile(t *testing.T) {
	t.Parallel()

	file, err := os.CreateTemp(t.TempDir(), "maintenance-*.html")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	if _, err := loadMaintenancePage(file.Name()); err == nil {
		t.Fatalf("expected error for empty maintenance file")
	}
}

func TestSiteRouterSetsXRealIP(t *testing.T) {
	t.Parallel()

	var gotRealIP string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRealIP = r.Header.Get("X-Real-IP")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute(upstream.URL),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	req.RemoteAddr = "203.0.113.50:12345"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if gotRealIP != "203.0.113.50" {
		t.Fatalf("expected X-Real-IP %q, got %q", "203.0.113.50", gotRealIP)
	}
}

func TestStatusRecorderCapturesStatus(t *testing.T) {
	t.Parallel()

	rec := &statusRecorder{
		ResponseWriter: httptest.NewRecorder(),
		status:         http.StatusOK,
	}

	rec.WriteHeader(http.StatusServiceUnavailable)
	n, err := rec.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if rec.status != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d", http.StatusServiceUnavailable, rec.status)
	}
	if rec.bytes != int64(n) {
		t.Fatalf("expected bytes %d, got %d", n, rec.bytes)
	}
}

func TestRequestIDGenerated(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute(upstream.URL),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	rid := recorder.Header().Get("X-Request-ID")
	if rid == "" {
		t.Fatal("expected X-Request-ID header to be set")
	}
	if len(rid) != 32 {
		t.Fatalf("expected 32-char hex request ID, got %q", rid)
	}
}

func TestRequestIDPreserved(t *testing.T) {
	t.Parallel()

	var gotID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID = r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute(upstream.URL),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	req.Header.Set("X-Request-ID", "existing-id-123")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if gotID != "existing-id-123" {
		t.Fatalf("expected upstream to receive existing ID %q, got %q", "existing-id-123", gotID)
	}
	if recorder.Header().Get("X-Request-ID") != "existing-id-123" {
		t.Fatalf("expected response to echo existing ID")
	}
}

func TestPathBasedRouting(t *testing.T) {
	t.Parallel()

	apiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "api")
	}))
	defer apiUpstream.Close()

	appUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "app")
	}))
	defer appUpstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"example.com": {
			upstreams: []upstreamEntry{
				{pathPrefix: "/api", upstream: apiUpstream.URL},
				{pathPrefix: "/", upstream: appUpstream.URL},
			},
			timeouts: TimeoutsConfig{}.withDefaults(),
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/api/users", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "api" {
		t.Fatalf("expected api, got %q", body)
	}

	req = httptest.NewRequest(http.MethodGet, "https://example.com/dashboard", nil)
	req.Host = "example.com"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ = io.ReadAll(rec.Result().Body)
	if string(body) != "app" {
		t.Fatalf("expected app, got %q", body)
	}
}

func TestTrustedProxyStripsHeaders(t *testing.T) {
	t.Parallel()

	var gotForwardedFor string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForwardedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	trusted, _ := parseTrustedProxies([]string{"10.0.0.0/8"})
	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": {
			upstreams:      []upstreamEntry{{pathPrefix: "/", upstream: upstream.URL}},
			timeouts:       TimeoutsConfig{}.withDefaults(),
			trustedProxies: trusted,
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	req.RemoteAddr = "203.0.113.50:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, req)

	if strings.Contains(gotForwardedFor, "1.2.3.4") {
		t.Fatalf("spoofed X-Forwarded-For should have been stripped, got %q", gotForwardedFor)
	}
	if !strings.Contains(gotForwardedFor, "203.0.113.50") {
		t.Fatalf("expected real client IP in X-Forwarded-For, got %q", gotForwardedFor)
	}
}

func TestCompressionMiddleware(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"hello":"world"}`))
	})

	handler := compressMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("expected Content-Encoding: gzip")
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	body, _ := io.ReadAll(gz)
	if string(body) != `{"hello":"world"}` {
		t.Fatalf("expected JSON body, got %q", body)
	}
}

func TestCompressionSkipsNonCompressible(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake png data"))
	})

	handler := compressMiddleware(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("image/png should not be gzip compressed")
	}
}

func TestRateLimitMiddleware(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := newRateLimitMiddleware(inner, 1, 0)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", rec.Code)
	}
}

func TestMaxBodyMiddleware(t *testing.T) {
	t.Parallel()

	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})

	handler := maxBodyMiddleware(inner, 5)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("this body is way too long"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if readErr == nil {
		t.Fatal("expected error reading oversized body")
	}
}

func TestHeaderManipulation(t *testing.T) {
	t.Parallel()

	var gotReqHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReqHeaders = r.Header.Clone()
		w.Header().Set("Server", "upstream")
		w.Header().Set("X-Keep", "yes")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": {
			upstreams: []upstreamEntry{{pathPrefix: "/", upstream: upstream.URL}},
			timeouts:  TimeoutsConfig{}.withDefaults(),
			requestHeaders: &HeaderOps{
				Set: map[string]string{"X-Custom-Req": "injected"},
			},
			responseHeaders: &HeaderOps{
				Remove: []string{"Server"},
				Set:    map[string]string{"X-Custom-Resp": "added"},
			},
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if gotReqHeaders.Get("X-Custom-Req") != "injected" {
		t.Fatalf("expected X-Custom-Req: injected, got %q", gotReqHeaders.Get("X-Custom-Req"))
	}

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.Header.Get("Server") != "" {
		t.Fatalf("expected Server header to be removed, got %q", resp.Header.Get("Server"))
	}
	if resp.Header.Get("X-Custom-Resp") != "added" {
		t.Fatalf("expected X-Custom-Resp: added, got %q", resp.Header.Get("X-Custom-Resp"))
	}
}

func TestStripPrefix(t *testing.T) {
	t.Parallel()

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"example.com": {
			upstreams: []upstreamEntry{{
				pathPrefix:  "/api",
				stripPrefix: "/api",
				upstream:    upstream.URL,
			}},
			timeouts: TimeoutsConfig{}.withDefaults(),
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/api/users", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotPath != "/users" {
		t.Fatalf("expected /users, got %q", gotPath)
	}
}

func TestAddPrefix(t *testing.T) {
	t.Parallel()

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"example.com": {
			upstreams: []upstreamEntry{{
				pathPrefix: "/",
				addPrefix:  "/v2",
				upstream:   upstream.URL,
			}},
			timeouts: TimeoutsConfig{}.withDefaults(),
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/users", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotPath != "/v2/users" {
		t.Fatalf("expected /v2/users, got %q", gotPath)
	}
}

func TestStripAndAddPrefix(t *testing.T) {
	t.Parallel()

	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"example.com": {
			upstreams: []upstreamEntry{{
				pathPrefix:  "/api",
				stripPrefix: "/api",
				addPrefix:   "/v1",
				upstream:    upstream.URL,
			}},
			timeouts: TimeoutsConfig{}.withDefaults(),
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/api/orders", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if gotPath != "/v1/orders" {
		t.Fatalf("expected /v1/orders, got %q", gotPath)
	}
}

func TestCORSMiddleware(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := corsMiddleware(inner, &CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "POST"},
		AllowedHeaders: []string{"Content-Type"},
		MaxAge:         3600,
	})

	// Preflight OPTIONS request.
	req := httptest.NewRequest(http.MethodOptions, "/api", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight: expected 204, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("expected origin reflection, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Header().Get("Access-Control-Allow-Methods") != "GET, POST" {
		t.Fatalf("expected methods, got %q", rec.Header().Get("Access-Control-Allow-Methods"))
	}

	// Normal GET with allowed origin.
	req = httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("expected CORS header on normal request")
	}

	// Request with disallowed origin.
	req = httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("Origin", "https://evil.com")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("should not set CORS header for disallowed origin")
	}
}

func TestCORSWildcard(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := corsMiddleware(inner, &CORSConfig{
		AllowedOrigins: []string{"*"},
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://anything.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected *, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestBasicAuthMiddleware(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secret")
	})

	hash, _ := bcrypt.GenerateFromPassword([]byte("pass123"), bcrypt.MinCost)
	handler := basicAuthMiddleware(inner, "admin", string(hash), "")

	// No credentials → 401.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("expected WWW-Authenticate header")
	}

	// Wrong password → 401.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "wrong")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// Correct credentials → 200.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.SetBasicAuth("admin", "pass123")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "secret" {
		t.Fatalf("expected secret, got %q", body)
	}
}

func TestRedirectMiddleware(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not redirected")
	})

	handler := redirectMiddleware(inner, []RedirectRule{
		{From: "/old", To: "/new", Status: 302},
	})

	// Matching path → redirect.
	req := httptest.NewRequest(http.MethodGet, "/old/page", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/new/page" {
		t.Fatalf("expected /new/page, got %q", loc)
	}

	// Non-matching → pass through.
	req = httptest.NewRequest(http.MethodGet, "/other", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRedirectDefaultStatus(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	handler := redirectMiddleware(inner, []RedirectRule{
		{From: "/old", To: "/new"},
	})

	req := httptest.NewRequest(http.MethodGet, "/old", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("expected 301 default, got %d", rec.Code)
	}
}

func TestMaxConnectionsMiddleware(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})

	handler := maxConnectionsMiddleware(inner, 1)

	// First request: occupies the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}()
	<-started

	// Second request while first is in-flight: should get 503.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}

	release <- struct{}{}
}

func TestMethodMatching(t *testing.T) {
	t.Parallel()

	postUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "post-handler")
	}))
	defer postUpstream.Close()

	getUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "get-handler")
	}))
	defer getUpstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"example.com": {
			upstreams: []upstreamEntry{
				{pathPrefix: "/api", methods: map[string]bool{"POST": true}, upstream: postUpstream.URL},
				{pathPrefix: "/api", upstream: getUpstream.URL},
			},
			timeouts: TimeoutsConfig{}.withDefaults(),
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	// POST /api → post handler.
	req := httptest.NewRequest(http.MethodPost, "https://example.com/api/data", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "post-handler" {
		t.Fatalf("expected post-handler, got %q", body)
	}

	// GET /api → get handler (fallback, no method filter).
	req = httptest.NewRequest(http.MethodGet, "https://example.com/api/data", nil)
	req.Host = "example.com"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ = io.ReadAll(rec.Result().Body)
	if string(body) != "get-handler" {
		t.Fatalf("expected get-handler, got %q", body)
	}
}

func TestErrorPageInterception(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "default 404")
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"example.com": {
			upstreams:  []upstreamEntry{{pathPrefix: "/", upstream: upstream.URL}},
			timeouts:   TimeoutsConfig{}.withDefaults(),
			errorPages: map[int][]byte{404: []byte("<h1>Custom Not Found</h1>")},
		},
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://example.com/missing", nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Custom Not Found") {
		t.Fatalf("expected custom error page, got %q", body)
	}
}

func TestWildcardHostMatching(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "wildcard")
	}))
	defer upstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"*.example.com": testRoute(upstream.URL),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	// sub.example.com matches *.example.com
	req := httptest.NewRequest(http.MethodGet, "https://sub.example.com/", nil)
	req.Host = "sub.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "wildcard" {
		t.Fatalf("expected wildcard, got %q", body)
	}

	// deep.sub.example.com also matches *.example.com
	req = httptest.NewRequest(http.MethodGet, "https://deep.sub.example.com/", nil)
	req.Host = "deep.sub.example.com"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ = io.ReadAll(rec.Result().Body)
	if string(body) != "wildcard" {
		t.Fatalf("expected wildcard for deep subdomain, got %q", body)
	}

	// other.org does NOT match
	req = httptest.NewRequest(http.MethodGet, "https://other.org/", nil)
	req.Host = "other.org"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-matching host, got %d", rec.Code)
	}
}

func TestExactHostTakesPriorityOverWildcard(t *testing.T) {
	t.Parallel()

	exactUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "exact")
	}))
	defer exactUpstream.Close()

	wildcardUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "wildcard")
	}))
	defer wildcardUpstream.Close()

	handler, _, err := newSiteRouter(map[string]siteRoute{
		"app.example.com": testRoute(exactUpstream.URL),
		"*.example.com":   testRoute(wildcardUpstream.URL),
	}, testLogger())
	if err != nil {
		t.Fatalf("newSiteRouter returned error: %v", err)
	}

	// Exact match wins.
	req := httptest.NewRequest(http.MethodGet, "https://app.example.com/", nil)
	req.Host = "app.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if string(body) != "exact" {
		t.Fatalf("expected exact, got %q", body)
	}

	// Other subdomain falls through to wildcard.
	req = httptest.NewRequest(http.MethodGet, "https://other.example.com/", nil)
	req.Host = "other.example.com"
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body, _ = io.ReadAll(rec.Result().Body)
	if string(body) != "wildcard" {
		t.Fatalf("expected wildcard, got %q", body)
	}
}

func TestHSTSMiddleware(t *testing.T) {
	t.Parallel()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := hstsMiddleware(inner, &HSTSConfig{
		MaxAge:            31536000,
		IncludeSubDomains: true,
		Preload:           true,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	got := rec.Header().Get("Strict-Transport-Security")
	expected := "max-age=31536000; includeSubDomains; preload"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestMatchHostFunction(t *testing.T) {
	t.Parallel()

	dummy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	exact := map[string]http.Handler{"app.example.com": dummy}
	wildcards := map[string]http.Handler{".example.com": dummy}

	if matchHost("app.example.com", exact, wildcards) == nil {
		t.Fatal("exact match should hit")
	}
	if matchHost("other.example.com", exact, wildcards) == nil {
		t.Fatal("wildcard match should hit")
	}
	if matchHost("example.com", exact, wildcards) != nil {
		t.Fatal("bare domain should not match wildcard")
	}
	if matchHost("evil.com", exact, wildcards) != nil {
		t.Fatal("unrelated domain should not match")
	}
}
