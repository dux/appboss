# deploy-boss

Bare-metal app host for one Linux box.
Runs the processes described by `dboss.yaml`, allocates ports, proxies HTTP to them, stops idle apps and wakes them on the next request, and keeps structured request logs per app.
It works in two modes with the same binary and the same file: inside one app folder as a Procfile replacement, or in a host folder that runs a directory of apps.

Sits directly behind the Cloudflare proxy as the origin; nothing else runs in front of it.
Replaces Caddy, lux-deploy's port allocator and unit renderer, and the Caddy-log-to-SQLite importer.
lux-deploy keeps rsync, releases, hooks and rollback and calls `dboss` at the end of a deploy.

Language: Go, stdlib plus `modernc.org/sqlite` and `gopkg.in/yaml.v3`.
Single static binary, one process.
`dboss start` always runs in the foreground; systemd is the daemonizer and `dboss systemd` writes the unit.

## Goals

* Symlink an app folder into the host's `apps/` directory and keep its process configuration with the app.
* Every app process gets `PORT` filled in.
  Ports are sticky across restarts.
* `dboss run|stop|restart|ls|logs|ports|rescan` CLI, all with `--json`.
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

`dboss.yaml` is the only config file.
A file with `procfile` describes an app; a file with `apps` describes a host that runs a directory of apps.
A file cannot be both.
Whenever a folder is resolved to its config, `dboss.local.yaml` is used when it exists and `dboss.yaml` otherwise.
The local file is server-only and gitignored, the same split as `.env.local` over `.env`.

Config lookup for every command: `-c path`, then `DBOSS_CONFIG`, then the current folder.

### Single mode (an app folder)

```
myapp/
  dboss.yaml        committed: procfile, hosts, overrides; may also carry proxy, ports, ... for standalone runs
  dboss.local.yaml  optional server-only replacement for dboss.yaml
  .env .env.local mise.toml
  .dboss/           state/, log/, dboss.sock (gitignored, created on first start)
```

`cd myapp && dboss start` runs this one app in the foreground with its output echoed as `myapp/web | ...`.
The app name is the folder name.
`run|stop|restart|status|logs` without an app name target the folder's app, so a deploy hook can call `dboss restart` from the release directory.

### Multi mode (a host folder)

```
/srv/dboss/
  dboss.yaml        apps: ./apps, proxy, management, ports, defaults, daemon
  apps/
    myapp         -> /apps/myapp
    myapp-staging -> /apps/myapp-staging
  .dboss/           or explicit state_dir, log_dir, socket
```

* Every entry of the `apps` directory is an app, named after the entry.
  Entries are symlinks to app folders or plain subfolders; dot entries are skipped.
* The entry path, not its target, is the process working directory, so a target that is itself a release symlink keeps working after a swap and restart.
* Apps are walked in name order; that fixes the initial port assignment.
* Adding an app is `ln -s /apps/new /srv/dboss/apps/new` followed by `dboss rescan`.
* An app's `dboss.yaml` under a host must not contain host keys (`proxy`, `ports`, `apps`, ...); such an app is reported as invalid.

### App folder contract

* Env is `.env` overlaid by `.env.local`.
* deploy-boss injects `PORT`, `APP_NAME`, `PROC_TYPE`, `DBOSS_SOCKET`, and the app's `PATH` resolved once via `mise env` when a `mise.toml` exists.
* Rescan is explicit (`dboss rescan`, or implicit on `dboss run <app>`).
* Rescan re-reads the apps directory and every app's config, or the root file itself in single mode.

### dboss.yaml (required, per app)

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

## Host config `/srv/dboss/dboss.yaml`

```yaml
apps: ./apps
socket: /run/dboss/dboss.sock   # default is ./.dboss/dboss.sock; the well-known path lets app folders find the host
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
`state_dir`, `log_dir` and `socket` default to `.dboss/` next to it.
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
* Store a pid file at `state_dir/<app>/<proctype>.pid` for humans and tooling.
* Every exit, ready, and health-failed event is bound to the process that produced it.
  An event from a process the runtime no longer tracks is dropped, so a late exit can never act on the replacement.

### Surviving a daemon restart

On startup, deploy-boss terminates every listener in `ports.range`, then starts fresh every app listed in `running.json`.
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
  Apps added by `dboss rescan` get the next free port.
* Always inject `PORT` into every configured process, including workers.
  There is no pinning or opt-out, and a `PORT` in `.env` is overwritten.
* The configured range is reserved exclusively for deploy-boss apps.
  At daemon startup, deploy-boss uses `lsof` to terminate every listener in the range before starting any app.
  Before every spawn it kills whatever still holds that process's port, so a stale process can never block a start.
* `dboss ports` lists the live table.
* The proxy and process both read the same table, so a mismatch is impossible.

## Proxy

`httputil.ReverseProxy` listens on the configured proxy address.
Per request:

1. Look up the host in the app table built from every app's `dboss.yaml` hosts.
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

The host session listens on a unix socket with a tiny JSON API.
The CLI is the same binary talking to that socket.
The management console calls the same manager in-process and never shells out.

```
dboss start [-c path]              run the host session in the foreground; Ctrl-C stops every app
dboss systemd [--install]          print the systemd unit for this config, or install and enable it
dboss ls [--json]                  apps, state, ports, uptime, last activity, mem
dboss run|stop|restart [app]       app defaults to the current folder's app
dboss status [app] [--json]        full detail incl. process list and restarts
dboss logs [app] [-f] [-n 200]     tail process logs
dboss ports                        live port table
dboss rescan                       re-read the apps directory and every dboss.yaml
dboss kill                         stop all apps and clear every listener in ports.range
dboss config [app] | check         print resolved config, validate without starting
```

Every command accepts `--json`.
Exit codes are meaningful for scripts.
Remote commands find the socket via `--socket`, `DBOSS_SOCKET`, the socket of the config in reach when it exists, then `/run/dboss/dboss.sock`.

When stdout is a terminal, `dboss start` echoes every process's output with a colored `app/proc | ` prefix, foreman style.
Under systemd stdout is not a terminal, so journald only receives dboss's own log and app output stays in `log_dir`.

## systemd

`dboss systemd` renders a unit that runs `dboss start -c <absolute config>` as the current user from the config directory, with `Restart=always`, `RuntimeDirectory=dboss` and `CAP_NET_BIND_SERVICE` for port 80.
`dboss systemd --install` writes `/etc/systemd/system/dboss.service`, reloads systemd and enables the service.
There is no static unit file in the repo because the paths differ per box.

## Management console

The daemon serves an embedded management console from a dedicated listener and hostname.
It shows live app state, resource use, request rates, process details, and start, stop, restart, and rescan controls.
The page refreshes app state every five seconds.

AuthCog protects only the management console.
The daemon completes the AuthCog callback server-side, checks the authenticated email against `management.auth.admin_emails`, and issues a signed host-only session cookie.
Mutation endpoints require a per-session CSRF token and same-origin request.
Proxied application traffic remains public and never enters the console authentication flow.

The console is served by the same listener as the apps: requests whose host is `management.host` go to the console, everything else to the app proxy.
The demo console is at `http://boss.lvh.me:8080`, next to the demo apps.

## lux-deploy integration

lux-deploy stops rendering systemd units and Caddy config.
After a symlink swap, it runs `dboss restart <app>` and `health.sh` can call `dboss status <app>`.
`host:apps` becomes `dboss ls`.
The Caddy log importer is retired.

## Repo layout

```
cmd/dboss/main.go         subcommand dispatch
internal/config/          dboss.yaml loading (host and app roles), defaults
internal/apps/            apps directory walk, env merge, procfile validation
internal/super/           app goroutine, state machine, spawn, port clearing, terminal echo
internal/res/             procgroup and cgroup backends
internal/ports/           in-memory port table
internal/proxy/           reverse proxy, starting page, host table
internal/console/         AuthCog-protected management API and embedded UI
internal/reqlog/          sqlite writer + prune
internal/ctl/             unix socket server + client
internal/cli/             command implementations, systemd unit generator
web/                      starting.html, crashed.html, 404.html
```

## Milestones

1. **Supervise.** App loading, env, procfile, ports, spawn, stop, restart, and `dboss start|ls|run|stop|restart|rescan|logs`.
   Runs on macOS.
2. **Proxy.** Host table, forward, starting page, readiness, idle stop.
3. **Logs.** SQLite request log, prune, `dboss status` shows request rates.
4. **Ops.** `dboss systemd`, management console, lux-deploy calls `dboss restart`.
5. **Later.** cgroup backend and per-app memory limits.

## Open questions

* Should `dboss ls` memory come from summing children (procgroup) and be labelled approximate until cgroups land? Yes, label it.
* Procfile commands with shell syntax (`&&`, `$VAR`) execute directly by default.
  Set `shell: true` in `dboss.yaml` to use `sh -c`.
* Multiple `web`-like process types behind the proxy: only `web` is routed.
  Others are background only.
