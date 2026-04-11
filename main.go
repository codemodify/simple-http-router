package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// reloadableHandler allows atomic handler swaps on config reload.
type reloadableHandler struct {
	mu      sync.RWMutex
	handler http.Handler
}

func (h *reloadableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	inner := h.handler
	h.mu.RUnlock()
	inner.ServeHTTP(w, r)
}

func (h *reloadableHandler) swap(newHandler http.Handler) {
	h.mu.Lock()
	h.handler = newHandler
	h.mu.Unlock()
}

// buildListenerRoutes extracts the per-host route map for a single listener.
// isTLS indicates whether this listener serves HTTPS (used for HSTS).
func buildListenerRoutes(cfg Config, listenerName string, isTLS bool) map[string]siteRoute {
	lr, ok := cfg.Routing[listenerName]
	if !ok {
		return nil
	}

	routes := make(map[string]siteRoute)
	for host, upstreamCfgs := range lr.Hosts {
		var entries []upstreamEntry
		for _, u := range upstreamCfgs {
			scheme := "http"
			if u.TLS {
				scheme = "https"
			}
			path := u.Path
			if path == "" {
				path = "/"
			}
			var methods map[string]bool
			if len(u.Methods) > 0 {
				methods = make(map[string]bool, len(u.Methods))
				for _, m := range u.Methods {
					methods[strings.ToUpper(m)] = true
				}
			}
			entries = append(entries, upstreamEntry{
				pathPrefix:  path,
				stripPrefix: u.StripPrefix,
				addPrefix:   u.AddPrefix,
				upstream:    scheme + "://" + u.To,
				methods:     methods,
				matchHeader: u.MatchHeader,
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			return len(entries[i].pathPrefix) > len(entries[j].pathPrefix)
		})

		resolved := cfg.resolveSite(host)
		siteTimeouts := TimeoutsConfig{}.withDefaults()
		if resolved.Timeouts != nil {
			siteTimeouts = resolved.Timeouts.withDefaults()
		}
		trusted, _ := parseTrustedProxies(resolved.TrustedProxies)
		errorPages, _ := loadErrorPages(resolved.ErrorPages)

		routes[host] = siteRoute{
			upstreams:           entries,
			maintenanceFile:     stringVal(resolved.MaintenanceFile),
			verbose:             boolVal(resolved.Verbose),
			compress:            boolVal(resolved.Compress),
			maxRequestBodyBytes: int64Val(resolved.MaxRequestBodyBytes),
			rateLimitRPS:        float64Val(resolved.RateLimitRPS),
			rateLimitBurst:      intVal(resolved.RateLimitBurst),
			maxIdleConns:        intVal(resolved.MaxIdleConns),
			timeouts:            siteTimeouts,
			trustedProxies:      trusted,
			requestHeaders:      resolved.RequestHeaders,
			responseHeaders:     resolved.ResponseHeaders,
			cors:                resolved.CORS,
			redirects:           resolved.Redirects,
			basicAuth:           resolved.BasicAuth,
			errorPages:          errorPages,
			hsts:                resolved.HSTS,
			isTLS:               isTLS,
		}
	}
	return routes
}

// listenerHosts returns the canonical host list for hosts on a given listener.
func listenerHosts(cfg Config, listenerName string) []string {
	lr, ok := cfg.Routing[listenerName]
	if !ok {
		return nil
	}
	var hosts []string
	for host := range lr.Hosts {
		hosts = append(hosts, canonicalHost(host))
	}
	return hosts
}

var version = "dev"

func run(args []string) error {
	fs := flag.NewFlagSet("simple-http-server-proxy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var configPath string
	var validateOnly bool
	var showVersion bool
	fs.StringVar(&configPath, "config", "config.json", "path to the JSON configuration file")
	fs.BoolVar(&validateOnly, "config-validate", false, "validate configuration and exit")
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}

	if validateOnly {
		fmt.Fprintln(os.Stderr, "configuration is valid")
		return nil
	}

	logLevel := &slog.LevelVar{}
	if boolVal(cfg.ConfigDefaults.Verbose) {
		logLevel.Set(slog.LevelDebug)
	} else {
		for _, sc := range cfg.Sites {
			if boolVal(sc.Verbose) {
				logLevel.Set(slog.LevelDebug)
				break
			}
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	timeouts := TimeoutsConfig{}.withDefaults()
	if cfg.ConfigDefaults.Timeouts != nil {
		timeouts = cfg.ConfigDefaults.Timeouts.withDefaults()
	}

	// Load static TLS certs for hosts on TLS listeners without auto-renew.
	// Uses a shared struct so SIGHUP can swap certs atomically.
	type certStore struct {
		mu    sync.RWMutex
		certs map[string]*tls.Certificate
	}
	certs := &certStore{certs: make(map[string]*tls.Certificate)}

	loadStaticCerts := func(c Config) error {
		newCerts := make(map[string]*tls.Certificate)
		for listenerName, lr := range c.Routing {
			if !c.Listeners[listenerName].TLS {
				continue
			}
			for host := range lr.Hosts {
				resolved := c.resolveSite(host)
				if resolved.Cert != nil && boolVal(resolved.Cert.Renew) {
					continue
				}
				if resolved.Cert == nil {
					continue
				}
				normalized := canonicalHost(host)
				if _, done := newCerts[normalized]; done {
					continue
				}
				loaded, err := tls.LoadX509KeyPair(resolved.Cert.TLSCert, resolved.Cert.TLSKey)
				if err != nil {
					return fmt.Errorf("site %q: load TLS cert/key: %w", host, err)
				}
				newCerts[normalized] = &loaded
			}
		}
		certs.mu.Lock()
		certs.certs = newCerts
		certs.mu.Unlock()
		return nil
	}

	if err := loadStaticCerts(cfg); err != nil {
		return err
	}

	// Build ACME managers for auto-renew sites.
	type managerKey struct {
		Email      string
		CacheDir   string
		UseStaging bool
	}

	allManagers := make(map[managerKey]*autocert.Manager)
	hostManager := make(map[string]*autocert.Manager)
	managerHostList := make(map[managerKey][]string)

	for _, lr := range cfg.Routing {
		for host := range lr.Hosts {
			normalized := canonicalHost(host)
			if _, done := hostManager[normalized]; done {
				continue
			}
			resolved := cfg.resolveSite(host)
			if resolved.Cert == nil || !boolVal(resolved.Cert.Renew) {
				continue
			}
			ra := resolved.Cert.RenewAccount
			useStaging := false
			if ra.UseStaging != nil {
				useStaging = *ra.UseStaging
			}
			key := managerKey{ra.Email, ra.CacheDir, useStaging}

			if allManagers[key] == nil {
				directoryURL := acme.LetsEncryptURL
				if key.UseStaging {
					directoryURL = letsEncryptStagingURL
				}
				allManagers[key] = &autocert.Manager{
					Prompt: autocert.AcceptTOS,
					Cache:  autocert.DirCache(key.CacheDir),
					Email:  key.Email,
					Client: &acme.Client{DirectoryURL: directoryURL},
				}
			}

			hostManager[normalized] = allManagers[key]
			managerHostList[key] = append(managerHostList[key], normalized)
		}
	}
	for key, m := range allManagers {
		m.HostPolicy = autocert.HostWhitelist(managerHostList[key]...)
	}

	// Create a server per listener.
	var allServers []*http.Server
	errCh := make(chan error, len(cfg.Listeners))
	reloadables := make(map[string]*reloadableHandler)

	for listenerName, listener := range cfg.Listeners {
		// HTTP listener with redirect_to_https: redirect all non-ACME traffic.
		if !listener.TLS && listener.RedirectToHTTPS {
			redirectStatus := listener.RedirectToHTTPSStatus
			if redirectStatus == 0 {
				redirectStatus = http.StatusMovedPermanently
			}
			redirectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				target := "https://" + r.Host + r.URL.RequestURI()
				http.Redirect(w, r, target, redirectStatus)
			})

			var httpHandler http.Handler = redirectHandler
			seen := make(map[*autocert.Manager]bool)
			for _, host := range listenerHosts(cfg, listenerName) {
				if m, ok := hostManager[host]; ok && !seen[m] {
					httpHandler = m.HTTPHandler(httpHandler)
					seen[m] = true
				}
			}

			server := &http.Server{
				Addr:              listener.Addr,
				Handler:           httpHandler,
				ReadHeaderTimeout: timeouts.ReadHeader.Duration,
				IdleTimeout:       timeouts.ServerIdle.Duration,
			}
			allServers = append(allServers, server)
			go serveHTTP(server, logger, errCh)
			continue
		}

		routes := buildListenerRoutes(cfg, listenerName, listener.TLS)
		if len(routes) == 0 {
			continue
		}

		handler, hosts, err := newSiteRouter(routes, logger)
		if err != nil {
			return err
		}

		rh := &reloadableHandler{}
		rh.handler = handler
		reloadables[listenerName] = rh

		// Wrap with connection limit if configured on this listener.
		var listenerHandler http.Handler = rh
		if listener.MaxConnections > 0 {
			listenerHandler = maxConnectionsMiddleware(rh, listener.MaxConnections)
		}

		if listener.TLS {
			var hasAutoRenew bool
			for _, host := range hosts {
				if _, ok := hostManager[host]; ok {
					hasAutoRenew = true
					break
				}
			}

			getCert := func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				host := canonicalHost(hello.ServerName)
				if m, ok := hostManager[host]; ok {
					return m.GetCertificate(hello)
				}
				certs.mu.RLock()
				cert, ok := certs.certs[host]
				certs.mu.RUnlock()
				if ok {
					return cert, nil
				}
				return nil, fmt.Errorf("no certificate for %s", hello.ServerName)
			}

			nextProtos := []string{"h2", "http/1.1"}
			if hasAutoRenew {
				nextProtos = []string{"h2", "http/1.1", acme.ALPNProto}
			}

			minVersion, _ := parseTLSVersion(listener.MinTLSVersion)
			tlsCfg := &tls.Config{
				MinVersion:     minVersion,
				GetCertificate: getCert,
				NextProtos:     nextProtos,
			}

			// mTLS: require and verify client certificates if client_ca is set.
			if listener.ClientCA != "" {
				caCert, err := os.ReadFile(listener.ClientCA)
				if err != nil {
					return fmt.Errorf("listener %q: read client CA %q: %w", listenerName, listener.ClientCA, err)
				}
				pool := x509.NewCertPool()
				if !pool.AppendCertsFromPEM(caCert) {
					return fmt.Errorf("listener %q: no valid certificates in client CA file %q", listenerName, listener.ClientCA)
				}
				tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
				tlsCfg.ClientCAs = pool
			}

			server := &http.Server{
				Addr:              listener.Addr,
				Handler:           listenerHandler,
				ReadHeaderTimeout: timeouts.ReadHeader.Duration,
				IdleTimeout:       timeouts.ServerIdle.Duration,
				TLSConfig:         tlsCfg,
			}
			allServers = append(allServers, server)
			go serveHTTPS(server, logger, errCh)
		} else {
			// HTTP listener: proxy traffic and handle ACME challenges.
			var httpHandler http.Handler = listenerHandler
			seen := make(map[*autocert.Manager]bool)
			for _, host := range hosts {
				if m, ok := hostManager[host]; ok && !seen[m] {
					httpHandler = m.HTTPHandler(httpHandler)
					seen[m] = true
				}
			}

			server := &http.Server{
				Addr:              listener.Addr,
				Handler:           httpHandler,
				ReadHeaderTimeout: timeouts.ReadHeader.Duration,
				IdleTimeout:       timeouts.ServerIdle.Duration,
			}
			allServers = append(allServers, server)
			go serveHTTP(server, logger, errCh)
		}
	}

	// Notify systemd that we're ready to serve traffic.
	sdNotify("READY=1")

	// Handle SIGHUP for config reload (routes, upstreams, timeouts).
	// Listener and TLS changes require a restart.
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			logger.Info("reloading configuration")
			newCfg, err := loadConfig(configPath)
			if err != nil {
				logger.Error("config reload failed", "error", err)
				continue
			}
			// Reload static TLS certificates.
			if err := loadStaticCerts(newCfg); err != nil {
				logger.Error("cert reload failed", "error", err)
			}
			for listenerName, rh := range reloadables {
				listenerIsTLS := newCfg.Listeners[listenerName].TLS
				routes := buildListenerRoutes(newCfg, listenerName, listenerIsTLS)
				if len(routes) == 0 {
					continue
				}
				newHandler, _, err := newSiteRouter(routes, logger)
				if err != nil {
					logger.Error("reload failed for listener", "listener", listenerName, "error", err)
					continue
				}
				rh.swap(newHandler)
			}
			logger.Info("configuration reloaded")
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeouts.Shutdown.Duration)
		defer cancel()
		for _, s := range allServers {
			_ = s.Shutdown(shutdownCtx)
		}
		return err
	case <-ctx.Done():
		logger.Info("shutdown requested")
		sdNotify("STOPPING=1")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeouts.Shutdown.Duration)
	defer cancel()

	var shutdownErr error
	for _, s := range allServers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("shutdown server %s: %w", s.Addr, err))
		}
	}

	return shutdownErr
}

func serveHTTP(server *http.Server, logger *slog.Logger, errCh chan<- error) {
	logger.Info("serving HTTP", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("HTTP server failed: %w", err)
	}
}

func serveHTTPS(server *http.Server, logger *slog.Logger, errCh chan<- error) {
	logger.Info("serving HTTPS", "addr", server.Addr)
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		errCh <- fmt.Errorf("HTTPS server failed: %w", err)
	}
}

// sdNotify sends a notification to systemd via the NOTIFY_SOCKET.
// No-op if NOTIFY_SOCKET is not set (i.e. not running under systemd).
func sdNotify(state string) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	addr := &net.UnixAddr{Name: sock, Net: "unixgram"}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(state))
}

const letsEncryptStagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"
