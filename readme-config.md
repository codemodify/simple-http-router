# Configuration Reference

The config file is plain JSON with four top-level keys:

```json
{
  "listeners":             { },
  "routing":               { },
  "sites-config-defaults": { },
  "sites-config-overrides": { }
}
```
- **listeners** - defines listeners
- **routing** - defines what host/path goes to what upstream
- **sites-config-defaults** - defaults for a site
- **sites-config-overrides** - overrides per ite


------


# Minimal working example

```json
{
  "listeners": { "all-on-80": { "addr": ":80" } },
  "routing": {
    "all-on-80": [
      { "host": "example1.com", "to": "127.0.0.1:8080" },
      { "host": "example2.com", "to": "127.0.0.1:8081" }
    ]
  }
}
```
- traffic -> `example1.com:80` -> `127.0.0.1:8080`
- traffic -> `example2.com:80` -> `127.0.0.1:8081`

------

# Listeners

Each listener is a named entry under `"listeners"`. The name is arbitrary (you reference it in routing).

```json
"listeners": {
  "all-on-80":  { "addr": ":80",   "redirect_to_https": true },
  "all-on-443": { "addr": ":443",  "tls": true, "max_connections": 10000 },
  "mtls":  { "addr": ":8443", "tls": true, "client_ca": "/etc/ssl/client-ca.pem", "min_tls_version": "1.3" }
}
```

how to read:
- `all-on-80` means ":80" which means all interfaces listening on 80
- `192-on-80` means "192.168.1.1:80" which means local interface with that IP listening on 80
- the label can be anything you need, it is semantics for easy read

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `addr` | string | required | listen address - `":80"`, `":443"`, `"0.0.0.0:8080"` |
| `tls` | bool | `false` | enable TLS on this listener |
| `redirect_to_https` | bool | `false` | redirect all traffic to HTTPS (don't combine with `tls: true`) |
| `redirect_to_https_status` | int | `301` | HTTP status for the redirect (must be 3xx) |
| `max_connections` | int | `0` | max concurrent connections, `0` = unlimited, returns 503 when exceeded |
| `client_ca` | string | `""` | path to PEM with client CA certs for mTLS |
| `min_tls_version` | string | `"1.2"` | minimum TLS version: `"1.2"` or `"1.3"` |

------

# Routing

Under `"routing"`, each key is a **listener name**. Under each listener you define which hosts it serves.

Two forms are supported:

**Simple** - array with `host` field (one upstream per host, good for most cases):

```json
"routing": {
  "all-on-80": [
    { "host": "example1.com", "to": "127.0.0.1:8080" },
    { "host": "example2.com", "to": "127.0.0.1:9090" }
  ]
}
```

**Multi-path** - object keyed by hostname, values are arrays (for path-based routing):

```json
"routing": {
  "all-on-443": {
    "app.example1.com": [
      { "path": "/api",  "to": "127.0.0.1:3000", "strip_prefix": "/api" },
      { "path": "/",     "to": "127.0.0.1:8080" }
    ]
  }
}
```

Hostnames support `*.example.com` wildcards. When multiple routes match, **longest path prefix wins**. Within the same path, `methods` and `match_header` narrow the match further.

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `host` | string | required (simple form) | hostname for this route |
| `to` | string | required | upstream address - `"127.0.0.1:8080"` |
| `tls` | bool | `false` | connect to upstream over HTTPS |
| `path` | string | `"/"` | path prefix to match (longest-prefix wins) |
| `strip_prefix` | string | `""` | remove this prefix before forwarding |
| `add_prefix` | string | `""` | prepend this prefix before forwarding |
| `methods` | []string | all | only match these HTTP methods - `["POST", "PUT"]` |
| `match_header` | string | `""` | only match when header equals value - `"X-Api-Version: v2"` |

------

# Site Config

`sites-config-defaults` sets baseline behavior for all hosts. `sites-config-overrides` lets you tweak individual hosts. Both have the same shape.

Override behavior: scalar values (bool, int, string) **replace** defaults. Objects like `timeouts` and `request_headers` **merge** (you only need to specify the keys you want to change). Arrays like `redirects` and `trusted_proxies` **replace** entirely.

```json
"sites-config-defaults": {
  "compress": true,
  "rate_limit_rps": 100,
  "timeouts": { "dial": "5s", "shutdown": "10s" }
},
"sites-config-overrides": {
  "app.example1.com": {
    "rate_limit_rps": 500,
    "timeouts": { "dial": "2s" }
  }
}
```

In this example `app.example1.com` gets `rate_limit_rps: 500` (replaced) and `timeouts: { "dial": "2s", "shutdown": "10s" }` (merged - `shutdown` carries over from defaults).

# General

| Key | Type | Default | Override | What it does |
|-----|------|---------|----------|--------------|
| `verbose` | bool | `false` | replace | debug logging for proxied requests |
| `site_under_maintenance_file` | string | `""` | replace | path to custom HTML maintenance page |
| `compress` | bool | `false` | replace | gzip text/JSON/XML responses |
| `max_request_body_bytes` | int | `0` | replace | max body size in bytes, `0` = unlimited, returns 413 |
| `rate_limit_rps` | float | `0` | replace | max requests/sec per client IP, `0` = disabled |
| `rate_limit_burst` | int | rps | replace | token bucket burst size |
| `max_idle_conns` | int | `100` | replace | max idle connections in the upstream pool |
| `trusted_proxies` | []string | `[]` | replace | CIDRs/IPs trusted for forwarding headers - `["10.0.0.0/8"]` |

# Timeouts

All values are duration strings like `"5s"`, `"500ms"`, `"2m"`. Override behavior: **merge**.

```json
"timeouts": {
  "dial": "5s",
  "keep_alive": "30s",
  "tls_handshake": "10s",
  "response_header": "30s",
  "idle_conn": "90s",
  "read_header": "10s",
  "server_idle": "120s",
  "shutdown": "10s"
}
```

| Key | Default | What it does |
|-----|---------|--------------|
| `dial` | `"5s"` | TCP connection timeout to upstream |
| `keep_alive` | `"30s"` | TCP keep-alive probe interval |
| `tls_handshake` | `"10s"` | TLS handshake timeout to upstream |
| `response_header` | `"30s"` | wait for upstream response headers |
| `idle_conn` | `"90s"` | max time an idle connection stays in pool |
| `read_header` | `"10s"` | wait for client to send request headers |
| `server_idle` | `"120s"` | max time between requests on a keep-alive connection |
| `shutdown` | `"10s"` | grace period for draining connections on shutdown |

# Header Operations

Apply to requests before forwarding or responses before returning. Override behavior: **merge**.

```json
"request_headers": {
  "set":    { "Host": "backend.internal" },
  "add":    { "X-Forwarded-By": "simple-http-router" },
  "remove": ["X-Powered-By"]
},
"response_headers": {
  "set":    { "X-Frame-Options": "DENY" },
  "remove": ["Server"]
}
```

| Key | Type | What it does |
|-----|------|--------------|
| `set` | map[string]string | set header to value (replaces existing) |
| `add` | map[string]string | append header value |
| `remove` | []string | remove headers by name |

# CORS

Override behavior: **replace** (entire block).

```json
"cors": {
  "allowed_origins": ["https://app.example1.com"],
  "allowed_methods": ["GET", "POST"],
  "allowed_headers": ["Content-Type", "Authorization"],
  "max_age": 3600
}
```

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `allowed_origins` | []string | -- | origins allowed to make requests (`"*"` = any) |
| `allowed_methods` | []string | `GET, POST, PUT, DELETE, OPTIONS, PATCH, HEAD` | allowed HTTP methods |
| `allowed_headers` | []string | `Content-Type, Authorization, X-Request-ID` | allowed request headers |
| `max_age` | int | `86400` | preflight cache duration in seconds |

# Redirects

Override behavior: **replace** (entire array).

Evaluated before proxying. The path suffix after `from` is appended to `to`.

```json
"redirects": [
  { "from": "/blog",    "to": "https://blog.example1.com", "status": 301 },
  { "from": "/old-api", "to": "/api",                     "status": 302 }
]
```

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `from` | string | required | path prefix to match |
| `to` | string | required | target URL or path |
| `status` | int | `301` | HTTP redirect status (must be 3xx) |

# Basic Auth

Override behavior: **replace**.

```json
"basic_auth": {
  "username": "admin",
  "password_hash": "$2a$10$...",
  "realm": "Admin Area"
}
```

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `username` | string | required | expected username |
| `password_hash` | string | required | bcrypt hash of the password |
| `realm` | string | `"Restricted"` | realm shown in browser auth dialog |

- generate pass with: `htpasswd -nbBC 10 "" 'your-password' | cut -d: -f2`

# Error Pages

Override behavior: **replace**.

```json
"error_pages": {
  "404": "/var/www/errors/404.html",
  "502": "/var/www/errors/502.html"
}
```

Keys are HTTP status codes as strings. Values are paths to HTML files served when the upstream returns that status.

# HSTS

Override behavior: **replace**. Only applied on TLS listeners.

```json
"hsts": {
  "max_age": 31536000,
  "include_subdomains": true,
  "preload": false
}
```

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `max_age` | int | `0` | seconds browsers should remember HTTPS-only, `0` = disabled |
| `include_subdomains` | bool | `false` | apply to all subdomains |
| `preload` | bool | `false` | allow browser HSTS preload list inclusion |

# TLS Certificates

Override behavior: **merge**. Use static cert files, ACME auto-renewal (Let's Encrypt), or both.

```json
"cert": {
  "no_renew_cert": "/etc/ssl/certs/site.crt",
  "no_renew_key":  "/etc/ssl/private/site.key"
}
```

or with auto-renewal:

```json
"cert": {
  "renew": true,
  "renew_account": {
    "email": "admin@example1.com",
    "cache_dir": "cert-cache",
    "use_staging": false
  }
}
```

| Key | Type | Default | What it does |
|-----|------|---------|--------------|
| `no_renew_cert` | string | `""` | path to static TLS certificate |
| `no_renew_key` | string | `""` | path to static TLS private key |
| `renew` | bool | `false` | enable ACME auto-renewal |
| `renew_account.email` | string | required | ACME registration email |
| `renew_account.cache_dir` | string | required | directory to cache certificates |
| `renew_account.use_staging` | bool | `false` | use Let's Encrypt staging (for testing) |

------

# CLI

```
simple-http-router -config config.json
simple-http-router -config-validate -config config.json
```

| Flag | Default | What it does |
|------|---------|--------------|
| `-config` | `config.json` | path to config file |
| `-config-validate` | `false` | validate config and exit (0 = valid, 1 = error) |

# Signals

| Signal | What it does |
|--------|--------------|
| `SIGHUP` | reload config (routes, site config, TLS certs) - listener changes require restart |
| `SIGINT` / `SIGTERM` | graceful shutdown (drains connections within `timeouts.shutdown`) |

# Systemd

Use `Type=notify` - the proxy sends `READY=1` after listeners are up and `STOPPING=1` on shutdown.

```ini
[Service]
Type=notify
ExecStart=/usr/local/bin/simple-http-router -config /etc/simple-http-router/config.json
ExecReload=/bin/kill -HUP $MAINPID
```
