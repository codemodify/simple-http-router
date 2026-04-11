package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Duration wraps time.Duration for JSON unmarshaling from strings like "5s", "100ms".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = dur
	return nil
}

type TimeoutsConfig struct {
	Dial           Duration `json:"dial"`
	KeepAlive      Duration `json:"keep_alive"`
	TLSHandshake   Duration `json:"tls_handshake"`
	ResponseHeader Duration `json:"response_header"`
	IdleConn       Duration `json:"idle_conn"`
	ReadHeader     Duration `json:"read_header"`
	ServerIdle     Duration `json:"server_idle"`
	Shutdown       Duration `json:"shutdown"`
}

func (t TimeoutsConfig) withDefaults() TimeoutsConfig {
	if t.Dial.Duration == 0 {
		t.Dial.Duration = 5 * time.Second
	}
	if t.KeepAlive.Duration == 0 {
		t.KeepAlive.Duration = 30 * time.Second
	}
	if t.TLSHandshake.Duration == 0 {
		t.TLSHandshake.Duration = 10 * time.Second
	}
	if t.ResponseHeader.Duration == 0 {
		t.ResponseHeader.Duration = 30 * time.Second
	}
	if t.IdleConn.Duration == 0 {
		t.IdleConn.Duration = 90 * time.Second
	}
	if t.ReadHeader.Duration == 0 {
		t.ReadHeader.Duration = 10 * time.Second
	}
	if t.ServerIdle.Duration == 0 {
		t.ServerIdle.Duration = 120 * time.Second
	}
	if t.Shutdown.Duration == 0 {
		t.Shutdown.Duration = 10 * time.Second
	}
	return t
}

// mergeWith returns a copy of t with non-zero fields from override applied.
func (t TimeoutsConfig) mergeWith(override TimeoutsConfig) TimeoutsConfig {
	if override.Dial.Duration != 0 {
		t.Dial = override.Dial
	}
	if override.KeepAlive.Duration != 0 {
		t.KeepAlive = override.KeepAlive
	}
	if override.TLSHandshake.Duration != 0 {
		t.TLSHandshake = override.TLSHandshake
	}
	if override.ResponseHeader.Duration != 0 {
		t.ResponseHeader = override.ResponseHeader
	}
	if override.IdleConn.Duration != 0 {
		t.IdleConn = override.IdleConn
	}
	if override.ReadHeader.Duration != 0 {
		t.ReadHeader = override.ReadHeader
	}
	if override.ServerIdle.Duration != 0 {
		t.ServerIdle = override.ServerIdle
	}
	if override.Shutdown.Duration != 0 {
		t.Shutdown = override.Shutdown
	}
	return t
}

type Config struct {
	Listeners      map[string]ListenerConfig   `json:"listeners"`
	ConfigDefaults SiteConfig                  `json:"sites-config-defaults"`
	Routing        map[string]ListenerRouting   `json:"routing"`
	Sites          map[string]SiteConfig        `json:"sites-config-overrides"`
}

type ListenerConfig struct {
	Addr                  string `json:"addr"`
	TLS                   bool   `json:"tls"`
	RedirectToHTTPS       bool   `json:"redirect_to_https"`
	RedirectToHTTPSStatus int    `json:"redirect_to_https_status"`
	MaxConnections        int    `json:"max_connections"`
	ClientCA              string `json:"client_ca"`
	MinTLSVersion         string `json:"min_tls_version"`
}

// HSTSConfig defines Strict-Transport-Security header settings.
type HSTSConfig struct {
	MaxAge            int  `json:"max_age"`
	IncludeSubDomains bool `json:"include_subdomains"`
	Preload           bool `json:"preload"`
}

// SiteConfig is the single type used for both global defaults and per-site overrides.
// Pointer fields allow distinguishing "not set" from "set to zero".
// When used as defaults, all fields are typically set.
// When used as overrides, only fields that differ from defaults are set.
type SiteConfig struct {
	Verbose             *bool             `json:"verbose"`
	MaintenanceFile     *string           `json:"site_under_maintenance_file"`
	Cert                *CertConfig       `json:"cert"`
	Timeouts            *TimeoutsConfig   `json:"timeouts"`
	TrustedProxies      []string          `json:"trusted_proxies"`
	MaxRequestBodyBytes *int64            `json:"max_request_body_bytes"`
	RateLimitRPS        *float64          `json:"rate_limit_rps"`
	RateLimitBurst      *int              `json:"rate_limit_burst"`
	MaxIdleConns        *int              `json:"max_idle_conns"`
	Compress            *bool             `json:"compress"`
	RequestHeaders      *HeaderOps        `json:"request_headers"`
	ResponseHeaders     *HeaderOps        `json:"response_headers"`
	CORS                *CORSConfig       `json:"cors"`
	Redirects           []RedirectRule    `json:"redirects"`
	BasicAuth           *BasicAuthConfig  `json:"basic_auth"`
	ErrorPages          map[string]string `json:"error_pages"`
	HSTS                *HSTSConfig       `json:"hsts"`
}

// resolveSite merges ConfigDefaults with per-site overrides for a given host.
// Headers and Timeouts are merged (site adds to defaults). All other fields
// are replaced entirely if the override sets them.
func (cfg Config) resolveSite(host string) SiteConfig {
	d := cfg.ConfigDefaults
	o, ok := cfg.Sites[host]
	if !ok {
		return d
	}

	// Simple overrides: site replaces default if non-nil/non-empty.
	if o.Verbose != nil {
		d.Verbose = o.Verbose
	}
	if o.MaintenanceFile != nil {
		d.MaintenanceFile = o.MaintenanceFile
	}
	if o.Compress != nil {
		d.Compress = o.Compress
	}
	if o.MaxRequestBodyBytes != nil {
		d.MaxRequestBodyBytes = o.MaxRequestBodyBytes
	}
	if o.RateLimitRPS != nil {
		d.RateLimitRPS = o.RateLimitRPS
	}
	if o.RateLimitBurst != nil {
		d.RateLimitBurst = o.RateLimitBurst
	}
	if o.MaxIdleConns != nil {
		d.MaxIdleConns = o.MaxIdleConns
	}
	if o.TrustedProxies != nil {
		d.TrustedProxies = o.TrustedProxies
	}
	if o.CORS != nil {
		d.CORS = o.CORS
	}
	if o.Redirects != nil {
		d.Redirects = o.Redirects
	}
	if o.BasicAuth != nil {
		d.BasicAuth = o.BasicAuth
	}
	if o.ErrorPages != nil {
		d.ErrorPages = o.ErrorPages
	}
	if o.HSTS != nil {
		d.HSTS = o.HSTS
	}

	// Merge: headers combine (site adds to/overrides defaults).
	d.RequestHeaders = mergeHeaderOps(d.RequestHeaders, o.RequestHeaders)
	d.ResponseHeaders = mergeHeaderOps(d.ResponseHeaders, o.ResponseHeaders)

	// Merge: timeouts merge (site overrides only specified fields).
	if o.Timeouts != nil {
		if d.Timeouts != nil {
			merged := d.Timeouts.mergeWith(*o.Timeouts)
			d.Timeouts = &merged
		} else {
			d.Timeouts = o.Timeouts
		}
	}

	// Merge: cert fields merge (site overrides only specified fields).
	if o.Cert != nil {
		d.Cert = mergeCertConfig(d.Cert, o.Cert)
	}

	return d
}

type CertConfig struct {
	TLSCert      string              `json:"no_renew_cert"`
	TLSKey       string              `json:"no_renew_key"`
	Renew        *bool               `json:"renew"`
	RenewAccount *RenewAccountConfig `json:"renew_account"`
}

type RenewAccountConfig struct {
	Email      string `json:"email"`
	CacheDir   string `json:"cache_dir"`
	UseStaging *bool  `json:"use_staging"`
}

func mergeCertConfig(def, override *CertConfig) *CertConfig {
	if def == nil {
		return override
	}
	if override == nil {
		return def
	}
	merged := *def
	if override.TLSCert != "" {
		merged.TLSCert = override.TLSCert
	}
	if override.TLSKey != "" {
		merged.TLSKey = override.TLSKey
	}
	if override.Renew != nil {
		merged.Renew = override.Renew
	}
	if override.RenewAccount != nil {
		if merged.RenewAccount == nil {
			merged.RenewAccount = override.RenewAccount
		} else {
			ra := *merged.RenewAccount
			if override.RenewAccount.Email != "" {
				ra.Email = override.RenewAccount.Email
			}
			if override.RenewAccount.CacheDir != "" {
				ra.CacheDir = override.RenewAccount.CacheDir
			}
			if override.RenewAccount.UseStaging != nil {
				ra.UseStaging = override.RenewAccount.UseStaging
			}
			merged.RenewAccount = &ra
		}
	}
	return &merged
}

func intVal(p *int) int           { if p != nil { return *p }; return 0 }
func boolVal(p *bool) bool       { if p != nil { return *p }; return false }
func stringVal(p *string) string  { if p != nil { return *p }; return "" }
func int64Val(p *int64) int64     { if p != nil { return *p }; return 0 }
func float64Val(p *float64) float64 { if p != nil { return *p }; return 0 }
func boolPtr(b bool) *bool       { return &b }

type UpstreamConfig struct {
	Host        string   `json:"host"`
	To          string   `json:"to"`
	TLS         bool     `json:"tls"`
	Path        string   `json:"path"`
	StripPrefix string   `json:"strip_prefix"`
	AddPrefix   string   `json:"add_prefix"`
	Methods     []string `json:"methods"`
	MatchHeader string   `json:"match_header"`
}

// ListenerRouting maps hostnames to upstream configurations for a single listener.
// JSON accepts two forms:
//   - Array of objects with "host" field (simple: one upstream per host)
//   - Object keyed by hostname, values are arrays or single upstream objects (multi-path)
type ListenerRouting struct {
	Hosts map[string][]UpstreamConfig
}

func (lr *ListenerRouting) UnmarshalJSON(data []byte) error {
	lr.Hosts = make(map[string][]UpstreamConfig)

	// Try as array first: [{"host": "example.com", "to": "..."}]
	var arr []UpstreamConfig
	if err := json.Unmarshal(data, &arr); err == nil {
		for _, u := range arr {
			if u.Host == "" {
				return fmt.Errorf("routing array entry missing required \"host\" field")
			}
			host := u.Host
			u.Host = "" // clear so it's not carried into upstream logic
			lr.Hosts[host] = append(lr.Hosts[host], u)
		}
		return nil
	}

	// Otherwise object keyed by hostname.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for host, val := range raw {
		// Try as array of upstreams first (path-based routing).
		var upstreams []UpstreamConfig
		if err := json.Unmarshal(val, &upstreams); err == nil {
			lr.Hosts[host] = upstreams
			continue
		}
		// Fall back to single upstream object.
		var upstream UpstreamConfig
		if err := json.Unmarshal(val, &upstream); err != nil {
			return fmt.Errorf("route %q: %w", host, err)
		}
		lr.Hosts[host] = []UpstreamConfig{upstream}
	}
	return nil
}

// CORSConfig defines CORS policy for a site.
type CORSConfig struct {
	AllowedOrigins []string `json:"allowed_origins"`
	AllowedMethods []string `json:"allowed_methods"`
	AllowedHeaders []string `json:"allowed_headers"`
	MaxAge         int      `json:"max_age"`
}

// RedirectRule defines a redirect from one path/host to another.
type RedirectRule struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status int    `json:"status"`
}

// BasicAuthConfig defines HTTP basic auth credentials.
type BasicAuthConfig struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Realm        string `json:"realm"`
}

// HeaderOps defines add/set/remove operations for HTTP headers.
type HeaderOps struct {
	Set    map[string]string `json:"set"`
	Add    map[string]string `json:"add"`
	Remove []string          `json:"remove"`
}

// mergeHeaderOps combines default-level and site-level header ops.
func mergeHeaderOps(def, site *HeaderOps) *HeaderOps {
	if def == nil && site == nil {
		return nil
	}
	if def == nil {
		return site
	}
	if site == nil {
		return def
	}
	merged := &HeaderOps{
		Set:    make(map[string]string),
		Add:    make(map[string]string),
		Remove: append([]string(nil), def.Remove...),
	}
	for k, v := range def.Set {
		merged.Set[k] = v
	}
	for k, v := range site.Set {
		merged.Set[k] = v
	}
	for k, v := range def.Add {
		merged.Add[k] = v
	}
	for k, v := range site.Add {
		merged.Add[k] = v
	}
	for _, v := range site.Remove {
		merged.Remove = append(merged.Remove, v)
	}
	return merged
}

func parseTLSVersion(s string) (uint16, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "1.2", "tls1.2":
		return tls.VersionTLS12, nil
	case "1.3", "tls1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unsupported TLS version %q (use \"1.2\" or \"1.3\")", s)
	}
}

func parseTrustedProxies(cidrs []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, cidr := range cidrs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			ip := net.ParseIP(cidr)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: not a valid CIDR or IP", cidr)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			network = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
		}
		nets = append(nets, network)
	}
	return nets, nil
}

func loadConfig(path string) (Config, error) {
	var cfg Config

	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func validateSiteConfig(sc SiteConfig, label string) error {
	if sc.TrustedProxies != nil {
		if _, err := parseTrustedProxies(sc.TrustedProxies); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	for _, r := range sc.Redirects {
		if r.From == "" || r.To == "" {
			return fmt.Errorf("%s: redirect must have non-empty 'from' and 'to'", label)
		}
		if r.Status != 0 && (r.Status < 300 || r.Status > 399) {
			return fmt.Errorf("%s: redirect status must be 3xx, got %d", label, r.Status)
		}
	}
	if sc.BasicAuth != nil {
		if sc.BasicAuth.Username == "" || sc.BasicAuth.PasswordHash == "" {
			return fmt.Errorf("%s: basic_auth requires username and password_hash", label)
		}
	}
	if sc.Cert != nil && sc.Cert.RenewAccount != nil {
		if strings.TrimSpace(sc.Cert.RenewAccount.Email) == "" {
			return fmt.Errorf("%s cert.renew_account.email is required for ACME registration", label)
		}
		if strings.TrimSpace(sc.Cert.RenewAccount.CacheDir) == "" {
			return fmt.Errorf("%s cert.renew_account.cache_dir must not be empty", label)
		}
	}
	return nil
}

func (cfg Config) validate() error {
	if len(cfg.Listeners) == 0 {
		return fmt.Errorf("config must include at least one listener")
	}

	for name, listener := range cfg.Listeners {
		if strings.TrimSpace(listener.Addr) == "" {
			return fmt.Errorf("listener %q must have a non-empty address", name)
		}
		if listener.TLS && listener.RedirectToHTTPS {
			return fmt.Errorf("listener %q: redirect_to_https cannot be used on a TLS listener", name)
		}
		if listener.RedirectToHTTPSStatus != 0 && (listener.RedirectToHTTPSStatus < 300 || listener.RedirectToHTTPSStatus > 399) {
			return fmt.Errorf("listener %q: redirect_to_https_status must be 3xx, got %d", name, listener.RedirectToHTTPSStatus)
		}
		if listener.MinTLSVersion != "" {
			if _, err := parseTLSVersion(listener.MinTLSVersion); err != nil {
				return fmt.Errorf("listener %q: %w", name, err)
			}
		}
	}

	if err := validateSiteConfig(cfg.ConfigDefaults, "sites-config-defaults"); err != nil {
		return err
	}

	if len(cfg.Routing) == 0 {
		return fmt.Errorf("config must include at least one listener in routing")
	}

	// Track which listeners have TLS, per host.
	hostOnTLSListener := make(map[string]string) // host → listener name (for error messages)

	for listenerName, lr := range cfg.Routing {
		if _, ok := cfg.Listeners[listenerName]; !ok {
			return fmt.Errorf("routing references unknown listener %q", listenerName)
		}
		if len(lr.Hosts) == 0 {
			return fmt.Errorf("routing listener %q must have at least one host", listenerName)
		}

		isTLS := cfg.Listeners[listenerName].TLS

		for host, upstreams := range lr.Hosts {
			normalized := canonicalHost(host)
			if normalized == "" {
				return fmt.Errorf("routing listener %q has an empty host key", listenerName)
			}
			for _, upstream := range upstreams {
				if strings.TrimSpace(upstream.To) == "" {
					return fmt.Errorf("routing listener %q host %q must have a non-empty \"to\"", listenerName, host)
				}
			}
			if isTLS {
				hostOnTLSListener[host] = listenerName
			}
		}
	}

	// Validate per-site overrides and cert requirements for all unique hosts.
	validated := make(map[string]bool)
	for _, lr := range cfg.Routing {
		for host := range lr.Hosts {
			if validated[host] {
				continue
			}
			validated[host] = true

			if _, ok := cfg.Sites[host]; ok {
				if err := validateSiteConfig(cfg.Sites[host], fmt.Sprintf("site %q", host)); err != nil {
					return err
				}
			}

			resolved := cfg.resolveSite(host)
			usesAutoRenew := resolved.Cert != nil && boolVal(resolved.Cert.Renew)

			if usesAutoRenew {
				if resolved.Cert.RenewAccount == nil || strings.TrimSpace(resolved.Cert.RenewAccount.Email) == "" {
					return fmt.Errorf("site %q cert.renew_account.email is required (set per-site or in sites-config-defaults)", host)
				}
				if strings.TrimSpace(resolved.Cert.RenewAccount.CacheDir) == "" {
					return fmt.Errorf("site %q cert.renew_account.cache_dir must not be empty (set per-site or in sites-config-defaults)", host)
				}
			}

			if !usesAutoRenew {
				if tlsListener, onTLS := hostOnTLSListener[host]; onTLS {
					if resolved.Cert == nil || resolved.Cert.TLSCert == "" || resolved.Cert.TLSKey == "" {
						return fmt.Errorf("host %q is on TLS listener %q without cert renew; tls_cert and tls_key are required (set per-site or in sites-config-defaults)", host, tlsListener)
					}
				}
			}
		}
	}

	return nil
}
