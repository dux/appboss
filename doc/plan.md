# app-boss

Bare-metal app host for one Linux box.
Runs the processes described by `appboss.yaml`, allocates ports, proxies HTTP to them, stops idle apps and wakes them on the next request, and keeps structured request logs per app.
It works in two modes with the same binary and the same file: inside one app folder as a Procfile replacement, or in a host folder that runs a directory of apps.

Sits directly behind the Cloudflare proxy as the origin; nothing else runs in front of it.
Replaces Caddy, lux-deploy's port allocator and unit renderer, and the Caddy-log-to-SQLite importer.
lux-deploy keeps rsync, releases, hooks and rollback and calls `appboss` at the end of a deploy.

Language: Go, stdlib plus `modernc.org/sqlite` and `gopkg.in/yaml.v3`.
Single static binary, one process.
`appboss start` always runs in the foreground; systemd is the daemonizer and `appboss systemd` writes the unit.

## Goals

* Symlink an app folder into the host's `apps/` directory and keep its process configuration with the app.
* Every app process gets `PORT` filled in.
  Ports are sticky across restarts.
* `appboss run|stop|restart|ls|logs|ports|rescan` CLI, all with `--json`.
* The built-in management console shows live app state and controls the supervisor through its in-process API.
* Idle apps stop after N hours without HTTP traffic and wake on the next request with a "starting, refresh in 5s" page.
* Per-app SQLite request log written by the proxy, pruned on a schedule.
* Works on macOS for development, Linux for production.

## Non-goals

* No Docker, no containers.
  Ever.
* No multi-host.
  One box, one daemon.
* No TLS, no HTTP/3, no rate limiting.
  Cloudflare owns the edge and talks plain HTTP to the box (Flexible mode).
* No deploy logic.
  Rsync, releases, hooks, rollback stay in lux-deploy.
* No DB provisioning (may come later as a separate command).

## Chain

```
Cloudflare -> boss :80 -> app :3100
```

Cloudflare passes the full request with the original `Host`, `CF-Connecting-IP` and `X-Forwarded-Proto`.
The daemon listens on port 80 (`CAP_NET_BIND_SERVICE` in the unit) and picks the app, or the management console, from the host header.
Blocking and body limits live in the Cloudflare WAF.
`proxy.trusted_cidrs` optionally rejects connections that do not come from Cloudflare, so the origin IP cannot be used to bypass it or spoof client IPs.
Websocket and other `Upgrade` connections are passed through as is.

## Two modes, one file

`appboss.yaml` is the only config file.
A file with `procfile` describes an app; a file with `apps` describes a host that runs a directory of apps.
A file cannot be both.
Whenever a folder is resolved to its config, `appboss.local.yaml` is used when it exists and `appboss.yaml` otherwise.
The local file is server-only and gitignored, the same split as `.env.local` over `.env`.

Config lookup for every command: `-c path`, then `APPBOSS_CONFIG`, then the current folder.

### Single mode (an app folder)

```
myapp/
  appboss.yaml        committed: procfile, hosts, overrides; may also carry proxy, ports, ... for standalone runs
  appboss.local.yaml  optional server-only replacement for appboss.yaml
  .env .env.local mise.toml
  .appboss/           state/, log/, appboss.sock (gitignored, created on first start)
```

`cd myapp && appboss start` runs this one app in the foreground with its output echoed as `myapp/web | ...`.
The app name is the folder name.
`run|stop|restart|status|logs` without an app name target the folder's app, so a deploy hook can call `appboss restart` from the release directory.

### Multi mode (a host folder)

```
/srv/appboss/
  appboss.yaml        apps: ./apps, proxy, management, ports, defaults, daemon
  apps/
    myapp         -> /apps/myapp
    myapp-staging -> /apps/myapp-staging
  .appboss/           or explicit state_dir, log_dir, socket
```

* Every entry of the `apps` directory is an app, named after the entry.
  Entries are symlinks to app folders or plain subfolders; dot entries are skipped.
* The entry path, not its target, is the process working directory, so a target that is itself a release symlink keeps working after a swap and restart.
* Apps are walked in name order; that fixes the initial port assignment.
* Adding an app is `ln -s /apps/new /srv/appboss/apps/new` followed by `appboss rescan`.
* An app's `appboss.yaml` under a host must not contain host keys (`proxy`, `ports`, `apps`, ...); such an app is reported as invalid.

### App folder contract

* Env is `.env` overlaid by `.env.local`.
* app-boss injects `PORT`, `APP_NAME`, `PROC_TYPE`, `APPBOSS_SOCKET`, and the app's `PATH` resolved once via `mise env` when a `mise.toml` exists.
* Rescan is explicit (`appboss rescan`, a save in the console, or implicit on `appboss run <app>`).
* Rescan re-reads the apps directory and every app's config, or the root file itself in single mode, and the host `defaults`.
  App-level keys apply live; a changed host key (`proxy`, `ports`, `apps`, `state_dir`, `log_dir`, `socket`, `management`, `daemon`) is reported as restart required and takes effect on the next `appboss start`.

### appboss.yaml (required, per app)

```yaml
procfile:
  web: bundle exec puma -C config/puma.rb
  worker: bundle exec lux jobs:work

hosts: [myapp.com, www.myapp.com]   # required for proxy routing
canonical_host: myapp.com           # every other host answers 301 here
static: ./public                    # served straight from disk, no wake
idle_stop: 6h                       # 0 = never sleep
health: http:/up                    # default: TCP connect on PORT
stop_timeout: 20s
restart: on-failure                 # on-failure | always | never
max_restarts: 5                     # in a row before marking crashed
```

`procfile` is required and maps each process name to its command.
Only `hosts` is additionally needed for a web app.
Everything else falls back to global defaults.
Every key under `defaults:` in the host file can be repeated at the top level of an app file, and the app value wins key by key.
Process keys (health, restart, logs, env, resources) can be overridden once more under `processes.<name>`; web keys (`static`, `basic_auth`, `allow_ips`, `max_body`, `headers`, `maintenance_page`) are app-level only.

## Host config `/srv/appboss/appboss.yaml`

```yaml
apps: ./apps
socket: /run/appboss/appboss.sock   # default is ./.appboss/appboss.sock; the well-known path lets app folders find the host
proxy:
  listen: ":80"
management:
  host: boss.example.com
  auth:
    realm: auth.authcog.com
    admin_emails: [admin@example.com]
    session_ttl: 24h
ports:
  range: [3100, 3990]
defaults:
  idle_stop: 6h
  stop_timeout: 20s
  restart: on-failure
  max_restarts: 5
  log_retention: 720h
```

Relative paths resolve from the directory containing the config.
`state_dir`, `log_dir` and `socket` default to `.appboss/` next to it.
Every key, its default and meaning, the full per-app format, env-file formats, and state files are documented in `internal/config/reference.yaml`.
That file is embedded in the binary, printed by `appboss config --reference` and shown in the console, so it never drifts from the release.

## Process model

One goroutine per app owns that app's processes and its state machine:

```
stopped -> starting -> running -> stopping -> stopped
                   \-> crashed (after max_restarts in a row)
```

Start, stop, restart, idle-timeout, and rescan are messages on the app's channel, so the idle timer and a CLI stop can never race.

Per process (one per `procfile` entry):

* Spawn with the configured app folder as cwd and merged env.
* Use `Setsid: true` so the child is its own session and process group.
* Pipe stdout and stderr to `log_dir/<app>/<proctype>.log` and keep the last N lines in memory for `appboss logs`.
* Block on `Wait` in the goroutine.
  On exit, record the code, apply restart policy with exponential backoff, and mark the app crashed after `max_restarts` consecutive failures.
* Stop with SIGTERM to the process group, wait `stop_timeout`, then send SIGKILL.
* Daemon shutdown stops all app process groups concurrently and waits for them before exiting.
* Store a pid file at `state_dir/<app>/<proctype>.pid` for humans and tooling.
* Every exit, ready, and health-failed event is bound to the process that produced it.
  An event from a process the runtime no longer tracks is dropped, so a late exit can never act on the replacement.

### Surviving a daemon restart

On startup, app-boss terminates every listener in `ports.range`, then starts fresh every app listed in `running.json`.
When no `running.json` exists yet, which is the case on a first start, every discovered app is started.
Nothing is adopted: the previous children were killed on shutdown, and anything left over is stale by definition.

### Resource backend (seam for cgroups)

Small interface: `Place(pid)`, `KillAll()`, `Stats() (mem, cpu)`.

* `procgroup` backend: Place is a no-op, KillAll signals the process group, and Stats sums known children.
  This is the default and works on macOS and Linux.
* `cgroup` backend (later, Linux only): Place writes pid to `/sys/fs/cgroup/boss/<app>/cgroup.procs`, KillAll writes to `cgroup.kill`, and Stats reads `memory.current` and `cpu.stat`.
  Limits are `memory.max` and `cpu.max`.
  It is chosen automatically when the cgroup dir is writable.

Nothing outside the app goroutine knows which backend is active.

## Ports

* Ports are assigned once at daemon startup and never change while the daemon runs.
  Apps are walked in `apps` order and their proctypes in name order, so the first configured app's `web` gets `ports.range[0]`.
  Apps added by `appboss rescan` get the next free port.
* Always inject `PORT` into every configured process, including workers.
  There is no pinning or opt-out, and a `PORT` in `.env` is overwritten.
* The configured range is reserved exclusively for app-boss apps.
  At daemon startup, app-boss uses `lsof` to terminate every listener in the range before starting any app.
  Before every spawn it kills whatever still holds that process's port, so a stale process can never block a start.
* `appboss ports` lists the live table.
* The proxy and process both read the same table, so a mismatch is impossible.

## Proxy

`httputil.ReverseProxy` listens on the configured proxy address.
Cloudflare owns TLS, HTTP/2 and 3, compression, caching, WAF and rate limiting; everything nginx or Caddy did in front of the app lives here.
Per request:

1. Look up the host in the app table built from every app's `appboss.yaml` hosts.
   Serve the boss 404 page for an unknown host.
   Every request that resolves to an app is logged, whatever step answers it.
2. `canonical_host`: a request for any other host of the app answers `301` to `<scheme>://<canonical><path?query>`, scheme from `X-Forwarded-Proto`, default `https`.
3. `allow_ips`: the client IP (`CF-Connecting-IP`, then the remote address) must fall in one of the CIDRs, else `403`.
4. `basic_auth`: `401` with `WWW-Authenticate: Basic realm="<app>"` unless the request carries a user from the map with a password matching its bcrypt hash.
   `appboss password` prints the hash.
5. Maintenance mode (`appboss maintenance <app> on`): `503` with `Retry-After: 30` and the maintenance page for HTML `GET`s, an empty `503` otherwise.
   The page is `maintenance_page`, then `<static>/503.html`, then the built-in one.
   The app keeps running; the flag is persisted in `state_dir/maintenance.json`.
6. `static`: `GET` and `HEAD` for a regular file under the directory are served from disk with `Last-Modified`, conditional and range support, without waking or touching the app.
   Paths under `static_immutable` get `Cache-Control: public, max-age=31536000, immutable`, everything else `public, max-age=3600`.
   The directory is opened through `os.OpenRoot` on every request, so `..` and symlinks cannot escape and a release symlink swap is picked up at once.
7. `max_body`: the request body is read in full before the app is contacted (in memory up to 1 MiB, larger bodies spill to a temp file removed once the request is done), so the app never sees a partial upload.
   A declared `Content-Length` or a chunked body over the limit answers `413` without forwarding.
8. For an app that is `running`, forward to its `web` port, stamp last activity, and apply `headers` to the response (an empty value removes the header).
9. App `stopped` or `crashed`: send a start message (idempotent), then:
   * A `GET` with `Accept: text/html` receives `503` with `Retry-After: 5` and a static starting page with the app name and a refresh timer.
     Crashed apps receive a different page and no auto-start loop.
   * Anything else receives `503`, `Retry-After: 5`, and an empty body.
10. App `starting`: same as 9 without sending another start.

`X-Request-ID` is set to `CF-Ray` when present, else 16 random bytes in hex, and forwarded to the app.
Path-based routing between apps stays out: it would double the host table and no app needs it.

Readiness uses a background poll after spawn.
It connects to `PORT` over TCP by default, or requests the configured HTTP health path until it returns 200 for apps that bind early.
The app then flips from `starting` to `running`.

Websockets and streaming pass through.
Open connections count as activity.

## Idle stop

A ticker runs every minute.
For each running app with `idle_stop > 0`, it stops the whole app when `now - last_activity > idle_stop`.
Apps that must never sleep set `idle_stop: 0`.

## Request log

Each app has a WAL-mode SQLite database at `log_dir/<app>/requests.sqlite`.
Columns are ts, method, host, path, status, duration_ms, bytes_out, ip (`CF-Connecting-IP`, then `X-Forwarded-For`, then remote addr), ua, and request_id.
Missing columns are added with `ALTER TABLE` when the database is opened.
Inserts are batched every second and rows older than `log_retention` are pruned daily.

## Control socket and CLI

The host session listens on a unix socket with a tiny JSON API.
The CLI is the same binary talking to that socket.
The management console calls the same manager in-process and never shells out.

```
appboss start [-c path]              run the host session in the foreground; Ctrl-C stops every app
appboss systemd [--install]          print the systemd unit for this config, or install and enable it
appboss ls [--json]                  apps, state, ports, uptime, last activity, mem
appboss run|stop|restart [app]       app defaults to the current folder's app
appboss status [app] [--json]        full detail incl. process list and restarts
appboss logs [app] [-f] [-n 200]     tail process logs
appboss maintenance [app] on|off     answer with the maintenance page while the app keeps running
appboss ports                        live port table
appboss rescan                       re-read the apps directory and every appboss.yaml
appboss kill                         stop all apps and clear every listener in ports.range
appboss config [app] [-d]            validate and print a config file as written, or with -d the resolved config
appboss config --reference           print the annotated configuration reference
appboss check                        validate the config and every app without starting
appboss password                     print a bcrypt hash for basic_auth
appboss help [command]               grouped overview, or synopsis, details and options of one command
```

Every command accepts `--json`.
Exit codes are meaningful for scripts.
Config errors name the file, line and key, and carry a hint: an unknown key suggests the closest valid one, a bad duration or size shows the accepted forms.
Remote commands find the socket via `--socket`, `APPBOSS_SOCKET`, the socket of the config in reach when it exists, then `/run/appboss/appboss.sock`.

When stdout is a terminal, `appboss start` echoes every process's output with a colored `app/proc | ` prefix, foreman style.
Under systemd stdout is not a terminal, so journald only receives appboss's own log and app output stays in `log_dir`.

## systemd

`appboss systemd` renders a unit that runs `appboss start -c <absolute config>` as the current user from the config directory, with `Restart=always`, `RuntimeDirectory=appboss` and `CAP_NET_BIND_SERVICE` for port 80.
`appboss systemd --install` writes `/etc/systemd/system/appboss.service`, reloads systemd and enables the service.
There is no static unit file in the repo because the paths differ per box.

## Management console

The daemon serves an embedded management console from a dedicated listener and hostname.
It shows live app state, resource use, request rates, process details, and start, stop, restart, maintenance, and rescan controls.
The page refreshes app state every five seconds.

Below the fleet, a configuration editor edits the real YAML files on disk: the host file and, per app, the file `appboss start` actually reads (`appboss.local.yaml` when it exists, else `appboss.yaml`).
There is no database copy of the config.
The editor validates through the real loaders, saves atomically with a revision check (a stale save shows the disk version side by side), rescans right away, and reports invalid apps and host keys that need a restart.
**Create server override** copies `appboss.yaml` to `appboss.local.yaml` so edits made on the box survive the next deploy.
**Effective config** shows the resolved app config and **Reference** the embedded annotated reference.

AuthCog protects only the management console.
The daemon completes the AuthCog callback server-side, checks the authenticated email against `management.auth.admin_emails`, and issues a signed host-only session cookie.
Mutation endpoints require a per-session CSRF token and same-origin request.
Proxied application traffic remains public and never enters the console authentication flow.

The console is served by the same listener as the apps: requests whose host is `management.host` go to the console, everything else to the app proxy.
It also owns the first port of `ports.range` on `127.0.0.1`, reserved before any app is discovered so app ports never shift, and answers there for the same hostname.
The demo console is at `http://boss.lvh.me:8080`, next to the demo apps, and at `http://boss.lvh.me:3100`.

## lux-deploy integration

lux-deploy stops rendering systemd units and Caddy config.
After a symlink swap, it runs `appboss restart <app>` and `health.sh` can call `appboss status <app>`.
`host:apps` becomes `appboss ls`.
The Caddy log importer is retired.

## Repo layout

```
cmd/appboss/main.go         subcommand dispatch
internal/config/          appboss.yaml loading (host and app roles), defaults, overrides, embedded reference
internal/apps/            apps directory walk, env merge, procfile validation, config file store
internal/super/           app goroutine, state machine, spawn, port clearing, terminal echo
internal/res/             procgroup and cgroup backends
internal/ports/           in-memory port table
internal/proxy/           reverse proxy, canonical redirect, allow list, basic auth, maintenance, static files, body limit
internal/console/         AuthCog-protected management API and embedded UI
internal/reqlog/          sqlite writer + prune
internal/ctl/             unix socket server + client
internal/cli/             command implementations, systemd unit generator
web/                      starting.html, crashed.html, 404.html, maintenance.html
```

## Milestones

1. **Supervise.** App loading, env, procfile, ports, spawn, stop, restart, and `appboss start|ls|run|stop|restart|rescan|logs`.
   Runs on macOS.
2. **Proxy.** Host table, forward, starting page, readiness, idle stop.
3. **Logs.** SQLite request log, prune, `appboss status` shows request rates.
4. **Ops.** `appboss systemd`, management console, lux-deploy calls `appboss restart`.
5. **Proxy features.** Canonical host, allow list, basic auth, maintenance mode, static files, body limit, response headers, request id; global-or-per-app config; console config editor.
6. **Later.** cgroup backend and per-app memory limits.
7. **Cron.** Per-app `cron:` jobs with `every <interval>` or 5-field cron schedules, run by the supervisor independently of the app's state, logged as a `cron-<job>` channel, with `appboss cron` and a console Run button.
8. **Deploy hooks.** Per-app `hooks:` with a signed `POST /hooks/<app>/<hook>` on the management host that runs a one-shot command in the app environment and can restart the app on success; the secret comes from the config or is generated under `state_dir`. `appboss hooks` lists, runs and rotates; `appboss exec` runs one-off commands. The command is operator-supplied, so lux-deploy still owns releases.
9. **Health and metrics.** `/healthz`, `/readyz` and Prometheus `/metrics` on the management host, rendered from the same snapshots the console shows (`./internal/metrics`); `management.metrics.enabled` and `.token` gate them.

## Open questions

* Should `appboss ls` memory come from summing children (procgroup) and be labelled approximate until cgroups land? Yes, label it.
* Procfile commands with shell syntax (`&&`, `$VAR`) execute directly by default.
  Set `shell: true` in `appboss.yaml` to use `sh -c`.
* Multiple `web`-like process types behind the proxy: only `web` is routed.
  Others are background only.
