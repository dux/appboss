# deploy-boss v2: proxy features, global-or-per-app config, console config editor

## Context

v1 runs apps, allocates ports, proxies by host, wakes idle apps and logs requests.
Everything nginx or Caddy did in front of a Rails or Bun app is still missing: static files, canonical host redirects, maintenance pages, body limits, basic auth for staging, response headers.
Cloudflare owns TLS, HTTP/2 and 3, compression, caching, WAF and rate limiting, so those stay out.

v2 adds the missing proxy features, makes every app-level key settable once for all apps or per app, and adds a configuration editor to the management console that edits the real `dboss.yaml` files on disk.

Three rules carry through the whole plan:

* One config surface.
  Every per-app key lives under `defaults:` in the host file and can be repeated at the top level of an app's `dboss.yaml`.
  The app value wins, key by key.
* Real files.
  The console edits the same YAML files the CLI reads; there is no database copy of the config.
* Apply live when possible.
  App-level changes apply through rescan without restarting the host session.
  Host-level changes that cannot apply live are reported as restart required.

## Part 1: config model

### 1.1 Two groups of app-level keys

`config.Defaults` today holds process keys (health, restart, logs, env, resources).
The new proxy keys are app-level but not process-level, so `Defaults` becomes two embedded groups, both flattened in YAML with `yaml:",inline"`:

```go
type Defaults struct {
    Process `yaml:",inline"`   // existing keys, overridable per process
    Web     `yaml:",inline"`   // new keys, app-level only
}
```

`Web` keys and defaults:

```yaml
defaults:
  static: ""                 # directory served straight from disk, relative to the app folder, e.g. ./public
  static_immutable: [/assets/]   # path prefixes under static that get max-age=31536000, immutable
  max_body: 0                # request body limit, 0 = none, e.g. 50m
  basic_auth: {}             # user -> bcrypt hash; empty = open
  allow_ips: []              # CIDRs allowed to reach the app; empty = everyone
  headers: {}                # response headers added to every app and static response; empty value removes the header
  maintenance_page: ""       # file served in maintenance mode; default <static>/503.html, then the built-in page
```

App-only keys stay app-only: `procfile`, `hosts`, `web_process`, `canonical_host`, `processes`.
They have no meaning across apps.

Global values for `basic_auth` and `allow_ips` are the staging-box case: one line protects every app.
Global `static: ./public` is the Rails and Vite convention; an app without a `public` folder is unaffected because a missing directory disables static serving for that app.

### 1.2 One override mechanism

Today every overridable key exists three times: in `Defaults`, in `appFile` as a pointer, and in `ProcessOverrides` as a pointer.
With seven new keys this becomes the main source of copy errors.

Replace it with one `Overrides` struct of pointer fields plus a small reflection helper in `./internal/config/overrides.go`:

```go
// apply copies every non-nil pointer field of overrides onto the field of the same name in target.
// Map fields merge key by key. Used for host defaults -> app and app -> process.
func apply(target any, overrides any)
```

* `appFile` embeds `Overrides` inline plus the app-only keys.
* `ProcessOverrides` is `Overrides` restricted to `Process` keys; a `Web` key under `processes.<name>` is a load error.
* A test asserts by reflection that `Overrides` and `Defaults` have the same field set, so a key can never be added to one and forgotten in the other.

`App.Process(name)` and `buildApp` shrink to a few lines each.

### 1.3 Effective config and live reload

* `config.Effective(app)` returns the fully resolved app config; `dboss config <app>` already prints this, the console reuses it.
* `Manager.Rescan` currently reloads only `Apps` and `App` from the root file.
  It also reloads `Defaults`, so a change to a global default reaches every app on the next rescan.
  A running app picks up the new spec through the existing `requestUpdate` path; process keys apply on the next process start, web keys apply immediately because the proxy reads them from the snapshot.
* `config.RestartRequired(old, new Config) []string` lists host keys that changed and cannot apply live: `proxy`, `management`, `ports`, `state_dir`, `log_dir`, `socket`, `daemon`, `apps`.
  Rescan logs them; the console shows them.

### 1.4 Snapshot carries what the proxy needs

`super.Snapshot` gains `Dir string`, `Web config.Web` and `Maintenance bool`.
`Web.BasicAuth` is `json:"-"` so hashes never reach the console or `dboss status --json`.
The proxy takes everything per request from `ResolveHost`, so a rescan changes behaviour without any proxy state.

## Part 2: proxy features

Request order inside `./internal/proxy/proxy.go`, after the existing trusted CIDR check and host lookup:

```
canonical redirect -> allow_ips -> basic_auth -> maintenance -> static -> max_body -> wake or forward -> headers
```

Each step is one small function with its own test in `proxy_test.go`.

### 2.1 Canonical host redirect

* `canonical_host: myapp.com` must be one of `hosts`.
* A request whose host (port stripped, case-insensitive) differs answers `301` to `<scheme>://<canonical><path?query>`.
* Scheme comes from `X-Forwarded-Proto`, default `https`.
* Runs before auth and static so `www` never serves content.

### 2.2 IP allowlist

* `allow_ips` parsed once at app load into `[]netip.Prefix`; invalid entries make the app invalid.
* Client IP is the existing `clientIP()` result, so `CF-Connecting-IP` decides, not the Cloudflare edge address.
* Rejected requests get `403` with the built-in 403 page for HTML clients and an empty body otherwise.

### 2.3 Basic auth

* `basic_auth: {alice: "$2a$10$..."}` with bcrypt hashes.
  New dependency `golang.org/x/crypto/bcrypt`.
* `401` with `WWW-Authenticate: Basic realm="<app>"` when missing or wrong.
  Comparison is bcrypt's own constant-time compare; user lookup is a map hit, which is fine because the user list is not secret.
* `dboss password` reads a password from the terminal without echo and prints the hash, so nobody has to find an `htpasswd`.
* Protects static files too, since it runs before the static step.

### 2.4 Maintenance mode

Runtime state, not config: the app keeps running, only the proxy answers differently.

* `dboss maintenance <app> on|off`, wire method `maintenance` in `./internal/ctl`.
  `[app]` defaults to the folder's app like the other commands.
* Persisted in `state_dir/maintenance.json` so it survives a host restart, mirroring `running.json`.
* Proxy serves `503` with `Retry-After: 30` and the maintenance page for HTML `GET`s, empty `503` otherwise.
  Page lookup: `maintenance_page` if set, then `<static>/503.html`, then the embedded `web/maintenance.html`.
* `dboss ls` shows `maintenance` in the STATE column; the console card gets a toggle next to start and stop.

### 2.5 Static files

* `static: ./public`, resolved against `Snapshot.Dir` on every request so a release symlink swap is picked up without a rescan.
* Only `GET` and `HEAD`.
  The path is cleaned and opened through `os.OpenRoot(staticDir)` so `..` cannot escape.
  Directories and missing files fall through to the app; there is no index file resolution.
* Served with `http.ServeContent`, which gives `Last-Modified`, `If-Modified-Since` and range requests for free.
* `Cache-Control`: `public, max-age=31536000, immutable` when the path starts with a `static_immutable` prefix, else `public, max-age=3600`.
* A static hit does not wake or touch the app, and is written to the request log like any other request.

### 2.6 Request body limit

* `max_body: 50m`.
  A `Content-Length` above the limit answers `413` before anything is read.
  Chunked bodies are wrapped in `http.MaxBytesReader`, and the reverse proxy error handler maps the resulting error to `413` instead of `502`.

### 2.7 Response headers

* `headers` applied through `ReverseProxy.ModifyResponse` for app responses and directly for static responses.
* An empty value removes the header, which is how an app drops `X-Powered-By` or a framework header.
* Redirects, auth challenges and error pages are not touched.

### 2.8 Request id in the log

* `X-Request-ID` is set to `CF-Ray` when present, else 16 random bytes hex, and forwarded to the app.
* `reqlog` gains a `request_id` column, added with `ALTER TABLE` on open when missing.
  `dboss status` output is unchanged; the console request table (Part 3) shows it.

### 2.9 Deferred

Path-based routing between apps stays out.
It doubles the host table and there is no app that needs it.

## Part 3: console config editor

### 3.1 What is editable

Everything.
The editor lists one entry per file dboss actually reads:

* `Host` - the root file `dboss start` was pointed at.
* one entry per app - the file `config.FindInDir` picks for that folder, so `dboss.local.yaml` when it exists, else `dboss.yaml`.

In single mode there is exactly one entry because the host file is the app file.

On a production box the committed `dboss.yaml` is overwritten by the next deploy.
The app entry therefore shows which file is active and offers **Create server override** when only `dboss.yaml` exists.
That copies `dboss.yaml` to `dboss.local.yaml` and switches the editor to it, so edits made on the server live in the file that survives deploys.

### 3.2 Store

New `./internal/apps/store.go` (it needs the app directory walk, which lives in this package):

```go
type ConfigFile struct {
    ID       string // "host" or "app:<name>"
    App      string
    Path     string // absolute
    Source   string // dboss.yaml or dboss.local.yaml
    Contents string
    Revision string // sha256 of Contents
    HasLocal bool   // app entries only
}

type Store struct { root config.Config }

func (s *Store) Files() ([]ConfigFile, error)
func (s *Store) Read(id string) (ConfigFile, error)
func (s *Store) Validate(id, contents string) error
func (s *Store) Write(id, contents, revision string) (ConfigFile, error)
func (s *Store) CreateLocal(app string) (ConfigFile, error)
```

* IDs map to paths server-side; the client never sends a path.
* `Validate` parses through the real loaders: `config.Parse(data, role)` is split out of `readFile` so validation runs on bytes without touching disk.
  The host file is validated as a root file, an app file as a child under the current defaults, with the same host-key rejection as at start.
* `Write` compares `revision` with the file on disk and answers a conflict when they differ, then writes `<name>.tmp` next to the file and renames it over, preserving the file mode.
* After a successful write the caller runs `Manager.Rescan` and returns its invalid list and the restart-required keys.

### 3.3 API

All under the existing admin session and CSRF rules in `./internal/console/console.go`:

```
GET  /api/config                      -> {files: [ConfigFile without Contents]}
GET  /api/config/file?id=app:sinatra  -> ConfigFile
POST /api/config/validate             -> {id, contents} -> {ok, error, line}
PUT  /api/config/file                 -> {id, contents, revision} -> {file, invalid: [], restart_required: []}   409 on revision mismatch
POST /api/config/local                -> {app} -> ConfigFile
GET  /api/config/effective?app=name   -> resolved YAML for the app (host defaults merged)
GET  /api/config/reference            -> the annotated reference (see 3.5)
```

`Handler` gets a `ConfigStore` interface next to `AppManager`; tests use an in-memory implementation.

### 3.4 UI

A **Configuration** section below the fleet in `index.html`, built in the existing vanilla `app.js` and `app.css`:

* Left: file list with the source name under each entry and a dot when the buffer is dirty.
* Center: a `<textarea>` editor in monospace with a line-number gutter, Tab inserting two spaces, and Cmd or Ctrl+S saving.
  No third-party editor; the files are short and the console has no build step.
* Toolbar: **Validate**, **Save**, **Effective config** (opens the resolved YAML for the selected app read-only), **Reference** (opens the annotated reference in a drawer), **Create server override** when applicable.
* Errors from validate or save show under the editor with the line highlighted when the YAML error carries one.
* After save: toast with the rescan result; a banner lists restart-required keys with the exact command to run (`systemctl restart dboss` or Ctrl-C in the terminal).
* A conflict answer reloads the file from disk into a second panel so the operator can copy their change over.

### 3.5 Reference

`doc/plan-config.yaml` moves to `internal/config/reference.yaml` and is embedded.
`dboss config --reference` prints it and the console serves it in the drawer, so the documentation ships in the binary and can never drift from the release.
`doc/plan.md` links to it.

## Part 4: CLI additions

```
dboss maintenance [app] on|off
dboss password                 print a bcrypt hash for basic_auth
dboss config --reference       print the annotated config reference
```

`dboss config <app>` keeps printing the effective config and is what the console's effective view calls.

## Part 5: order of work

Each step leaves `make check` green and the demo working.

1. Config model: `Process` and `Web` groups, `Overrides` with `apply`, field-set test, `config.Parse`, rescan reloads defaults, `RestartRequired`.
2. Snapshot fields `Dir`, `Web`, `Maintenance`; proxy steps 2.1, 2.2, 2.7, 2.6 (no new deps).
3. Basic auth and `dboss password` (adds `x/crypto`).
4. Static files with the demo sinatra app gaining a `public/` folder and `static: ./public`.
5. Maintenance mode end to end: state, ctl, CLI, proxy, console toggle, built-in page.
6. Request id column in `reqlog`.
7. Config store and API with tests.
8. Console editor UI.
9. Reference embedding, `plan.md` and `reference.yaml` updates.

## Verification

* `make check` after every step.
* Demo host (`make demo`) with sinatra given `public/robots.txt`, `public/assets/app.css`, `canonical_host: sinatra.lvh.me` and a second host `www.sinatra.lvh.me`:
  * `curl -H 'Host: www.sinatra.lvh.me'` answers `301` to `https://sinatra.lvh.me/`.
  * `curl -H 'Host: sinatra.lvh.me' /assets/app.css` answers `200` with the immutable cache header while `dboss ls` still shows sinatra stopped.
  * `dboss maintenance sinatra on` makes `/` answer `503` with the page and `dboss ls` show `maintenance`; `off` restores it.
  * `basic_auth` set globally in `demo/dboss.yaml`: `/` answers `401`, then `200` with `-u`.
  * `allow_ips: [10.0.0.0/8]` with `CF-Connecting-IP: 10.1.1.1` passes and `203.0.113.1` gets `403`.
  * a `Content-Length` above `max_body` answers `413`.
* Console at `http://boss.lvh.me:8080`: edit `demo/apps/bun/dboss.yaml` to add a host, save, see the rescan toast and the new host on the card; edit `demo/dboss.yaml` `defaults.idle_stop`, save, see it in the effective view; change `proxy.listen`, save, see the restart-required banner; save with a stale revision from a second tab and get the conflict panel; create a server override for sinatra and confirm `dboss.local.yaml` exists and is the active file.
* `curl` a request with `CF-Ray: abc` and confirm the row in `requests.sqlite` carries it.
