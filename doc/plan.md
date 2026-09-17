# deploy-boss

Bare-metal app host for one Linux box.
Loads an explicit list of app folders, runs the processes in each app's `deploy-boss.yaml`, allocates ports, proxies HTTP to them, stops idle apps and wakes them on the next request, and keeps structured request logs per app.

Sits under nginx (or Cloudflare directly).
Replaces Caddy, lux-deploy's port allocator and unit renderer, and the Caddy-log-to-SQLite importer.
lux-deploy keeps rsync, releases, hooks and rollback and calls `dboss` at the end of a deploy.

Language: Go, stdlib plus `modernc.org/sqlite` and `gopkg.in/yaml.v3`.
Single static binary, one process, subcommands for daemon and CLI.

## Goals

* Add an app folder to the global `apps` list and keep its process configuration with the app.
* Every app process gets `PORT` filled in.
  Ports are sticky across restarts.
* `dboss start|stop|restart|ls|logs|ports|rescan` CLI, all with `--json`.
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
  Cloudflare and/or nginx own the edge.
* No deploy logic.
  Rsync, releases, hooks, rollback stay in lux-deploy.
* No DB provisioning (may come later as a separate command).

## Chain

```
Cloudflare -> nginx :80 -> boss proxy :8080 -> app :3100
```

nginx keeps one static `location /` to the proxy plus the crude blocklist, body limits, and `proxy_set_header Host/X-Forwarded-*`.
It never learns about individual apps.
If the box sits behind Cloudflare Tunnel, nginx is optional.

## App folder contract

```
/apps/<name>/
  deploy-boss.yaml    required process and app configuration
  .env                committed defaults
  .env.local          server-only overrides, wins over .env
  mise.toml           optional tool environment
```

* App name is the final folder name.
* Unit name is `boss-<app>-<proctype>`.
* The listed folder is the process working directory and may itself be a release symlink.
* Env is `.env` overlaid by `.env.local`.
* deploy-boss injects `PORT`, `APP_NAME`, `PROC_TYPE`, and the app's `PATH` resolved once via `mise env` when a `mise.toml` exists.
* Rescan is explicit (`dboss rescan`, or implicit on `dboss start <app>`).
* Rescan reloads the central app list and every app's `deploy-boss.yaml`.

### deploy-boss.yaml (required, per app)

```yaml
procfile:
  web: bundle exec puma -C config/puma.rb
  worker: bundle exec lux jobs:work

hosts: [myapp.com, www.myapp.com]   # required for proxy routing
idle_stop: 6h                       # 0 = never sleep
health: http:/up                    # default: TCP connect on PORT
stop_timeout: 20s
restart: on-failure                 # on-failure | always | never
max_restarts: 5                     # in a row before marking crashed
```

`procfile` is required and maps each process name to its command.
Only `hosts` is additionally needed for a web app.
Everything else falls back to global defaults.

## Global config `/etc/boss/deploy-boss.config.yaml`

```yaml
apps:
  - /apps/myapp
  - /apps/myapp-staging
state_dir: /var/lib/boss
log_dir: /var/log/boss
socket: /run/boss/boss.sock
proxy:
  listen: 127.0.0.1:8080
management:
  listen: 127.0.0.1:8081
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

Relative app and runtime paths resolve from the directory containing the global config.
Every key, its default and meaning, the full per-app format, env-file formats, and state files are documented in `plan-config.yaml` next to this file.
That file is the authoritative config reference.

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
* Pipe stdout and stderr to `log_dir/<app>/<proctype>.log` and keep the last N lines in memory for `dboss logs`.
* Block on `Wait` in the goroutine.
  On exit, record the code, apply restart policy with exponential backoff, and mark the app crashed after `max_restarts` consecutive failures.
* Stop with SIGTERM to the process group, wait `stop_timeout`, then send SIGKILL.
* Daemon shutdown stops all app process groups concurrently and waits for them before exiting.
* Store a pid file at `state_dir/<app>/<proctype>.pid` for adoption.

### Surviving a daemon restart

On startup, check that each pid is alive and `/proc/<pid>/cmdline` (or `ps` on macOS) matches the expected command.
If so, adopt it, mark it running, poll `kill -0` every 5s until it exits, then respawn it as a real child.
Otherwise, remove the stale pid file.

### Resource backend (seam for cgroups)

Small interface: `Place(pid)`, `KillAll()`, `Stats() (mem, cpu)`.

* `procgroup` backend: Place is a no-op, KillAll signals the process group, and Stats sums known children.
  This is the default and works on macOS and Linux.
* `cgroup` backend (later, Linux only): Place writes pid to `/sys/fs/cgroup/boss/<app>/cgroup.procs`, KillAll writes to `cgroup.kill`, and Stats reads `memory.current` and `cpu.stat`.
  Limits are `memory.max` and `cpu.max`.
  It is chosen automatically when the cgroup dir is writable.

Nothing outside the app goroutine knows which backend is active.

## Ports

* Allocate the first free port per (app, proctype) from `ports.range` and persist it in `state_dir/ports.json`.
  Never reassign it while the entry exists.
* Always inject `PORT` into every configured process, including workers.
  There is no pinning or opt-out, and a `PORT` in `.env` is overwritten.
* The configured range is reserved exclusively for deploy-boss apps.
  At daemon startup, deploy-boss uses `lsof` to terminate every listener in the range before starting any app.
* `dboss ports` lists allocations.
  `dboss ports release <app>` frees them only when the app is stopped.
* The proxy and process both read the same table, so a mismatch is impossible.

## Proxy

`httputil.ReverseProxy` listens on the configured proxy address.
Per request:

1. Look up the host in the app table built from every app's `deploy-boss.yaml` hosts.
   Serve the boss 404 page for an unknown host.
2. For an app that is `running`, forward to its `web` port, stamp last activity, and log the request.
3. App `stopped` or `crashed`: send a start message (idempotent), then:
   * A `GET` with `Accept: text/html` receives `503` with `Retry-After: 5` and a static starting page with the app name and a refresh timer.
     Crashed apps receive a different page and no auto-start loop.
   * Anything else receives `503`, `Retry-After: 5`, and an empty body.
4. App `starting`: same as 3 without sending another start.

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
Columns are ts, method, host, path, status, duration_ms, bytes_out, ip (`CF-Connecting-IP`, then `X-Forwarded-For`, then remote addr), and ua.
Inserts are batched every second and rows older than `log_retention` are pruned daily.

## Control socket and CLI

The daemon listens on a unix socket with a tiny JSON API.
The CLI is the same binary talking to that socket.
The management console calls the same manager in-process and never shells out.

```
dboss daemon                       run the daemon (systemd unit, Restart=always)
dboss ls [--json]                  apps, state, ports, uptime, last activity, mem
dboss start|stop|restart <app>
dboss kill                         stop all apps and clear every listener in ports.range
dboss rescan                       re-read the apps list and deploy-boss.yaml files
dboss logs <app> [-f] [-n 200]     tail process logs
dboss ports [release <app>]
dboss status <app> [--json]        full detail incl. process list and restarts
```

Every command accepts `--json`.
Exit codes are meaningful for scripts.

## Management console

The daemon serves an embedded management console from a dedicated listener and hostname.
It shows live app state, resource use, request rates, process details, and start, stop, restart, and rescan controls.
The page refreshes app state every five seconds.

AuthCog protects only the management console.
The daemon completes the AuthCog callback server-side, checks the authenticated email against `management.auth.admin_emails`, and issues a signed host-only session cookie.
Mutation endpoints require a per-session CSRF token and same-origin request.
Proxied application traffic remains public and never enters the console authentication flow.

nginx routes the management hostname to `management.listen` and all app hostnames to `proxy.listen`.
The two listeners must use different ports and neither may overlap the application port range.
The demo console is available directly at `http://boss.lvh.me:8081`; its app proxy remains at port 8080.

## lux-deploy integration

lux-deploy stops rendering systemd units and Caddy config.
After a symlink swap, it runs `dboss restart <app>` and `health.sh` can call `dboss status <app>`.
`host:apps` becomes `dboss ls`.
The Caddy log importer is retired.

## Repo layout

```
cmd/dboss/main.go         subcommand dispatch
internal/config/          global and per-app YAML loading, defaults
internal/apps/            app list loading, env merge, procfile validation
internal/super/           app goroutine, state machine, spawn, adopt
internal/res/             procgroup and cgroup backends
internal/ports/           allocator + ports.json
internal/proxy/           reverse proxy, starting page, host table
internal/console/         AuthCog-protected management API and embedded UI
internal/reqlog/          sqlite writer + prune
internal/ctl/             unix socket server + client
internal/cli/             command implementations
web/                      starting.html, crashed.html, 404.html
deploy/boss.service       systemd unit for the daemon
deploy/nginx.conf         reference nginx snippet
```

## Milestones

1. **Supervise.** App loading, env, procfile, ports, spawn, stop, restart, adoption, and `dboss daemon|ls|start|stop|restart|rescan|logs`.
   Runs on macOS.
2. **Proxy.** Host table, forward, starting page, readiness, idle stop.
3. **Logs.** SQLite request log, prune, `dboss status` shows request rates.
4. **Ops.** systemd unit, nginx snippet, management console, lux-deploy calls `dboss restart`.
5. **Later.** cgroup backend and per-app memory limits.

## Open questions

* Should `dboss ls` memory come from summing children (procgroup) and be labelled approximate until cgroups land? Yes, label it.
* Procfile commands with shell syntax (`&&`, `$VAR`) execute directly by default.
  Set `shell: true` in `deploy-boss.yaml` to use `sh -c`.
* Multiple `web`-like process types behind the proxy: only `web` is routed.
  Others are background only.
