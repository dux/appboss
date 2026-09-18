# deploy-boss

Bare-metal app host for one Linux box, in a single Go binary called `dboss`.
It runs the processes described by each app's `dboss.yaml`, hands every process a fixed `PORT`, proxies HTTP to the right app by hostname, stops idle apps and wakes them on the next request, and keeps a per-app SQLite request log.
A built-in management console shows live state, controls the supervisor and edits the config files on disk.

It sits directly behind Cloudflare as the origin.
There is no TLS, no containers and no deploy logic; rsync, releases and rollback stay in lux-deploy, which calls `dboss` at the end of a deploy.
The design documents are `./doc/plan.md` (v1) and `./doc/plan-v2.md` (proxy features, shared defaults, console config editor).

## Requirements

* Go 1.25 or newer to build.
* Linux for production, macOS for development.
* Nothing else at runtime: the binary embeds the console assets and the configuration reference.

## Build and run the demo

```sh
make build            # ./bin/dboss
make demo             # builds, then runs the host session on ./demo/dboss.yaml
```

The demo listens on `127.0.0.1:8080` and hosts two apps.
Use the hostnames with the port; the bare hostnames refuse to connect.

* http://boss.lvh.me:8080 - management console
* http://sinatra.lvh.me:8080 - Ruby app (needs the Ruby from `./demo/apps/sinatra/mise.toml` and `bundle install`)
* http://bun.lvh.me:8080 - Bun app

`make demo-watch` rebuilds and restarts on source changes through `watchexec`.
`make kill` stops the demo apps and clears the port range after a crash.

## One config file, two modes

`dboss.yaml` is the only configuration file.
A file with `procfile` describes an app; a file with `apps` describes a host that runs a directory of apps.
`dboss.local.yaml` next to it wins when it exists and is meant for server-only overrides (gitignored).
Every command looks for the config as `-c path`, then `$DBOSS_CONFIG`, then the current folder.

Host file (`./demo/dboss.yaml`):

```yaml
apps: ./apps

proxy:
  listen: 127.0.0.1:8080

management:
  host: boss.lvh.me
  auth:
    realm: auth.authcog.com
    admin_emails:
      - you@example.com
    session_ttl: 24h

ports:
  range: [3100, 3199]

defaults:
  idle_stop: 0s
```

App file (`./demo/apps/bun/dboss.yaml`):

```yaml
procfile:
  web: ./start.sh

hosts:
  - bun.lvh.me

health: http:/up
```

The proxy listens on `:80` by default and owns that port for every app; the demo sets `127.0.0.1:8080` so a hand-run session needs no capability.
Every key that takes a list also accepts a single value, so `hosts: myapp.com` equals `hosts: [myapp.com]`.
`proxy.listen` and `management.host` are such lists: several listen addresses each get a listener with the same routing, and several console hostnames are all accepted.
Every app-level key can be set once under `defaults:` in the host file and repeated at the top level of an app file; the app value wins key by key.
`dboss config --keys [filter]` lists every key with a one-line description and its default, or an example when it has none; the same list is behind the Help button in the console's Configuration view.
`dboss config --reference` prints the long annotated reference, and `dboss config [app] -d` prints a resolved config with every default filled in.

```
$ dboss config --keys health
Shared app keys  (defaults: in the host file, top level in an app file; per-process ones also under processes.<name>)
  health           readiness check: tcp, or http:<path> expecting 2xx               tcp    per process
  health_interval  poll interval of the readiness check                             500ms  per process
  health_timeout   give-up time of the readiness check; counts as a failed restart  1m     per process
```

## Commands

```
dboss <command> [options]
dboss help <command>

Host session
  start         run the host session in the foreground; Ctrl-C stops every app
  systemd       print the systemd unit for this config, or install and enable it
  kill          stop every app and terminate every listener left in ports.range
  login         print a one-time console URL that signs you in as cli@localhost

Apps
  ls            list apps with state, ports, uptime, last activity and memory
  run           start an app; rescans first when it is not known yet
  stop          stop an app and keep it stopped until run or the next request
  restart       stop and start an app on the same ports
  status        full detail for one app: processes, restarts, resources, request rates
  logs          print or follow the process logs of an app
  maintenance   answer every request with the maintenance page while the app keeps running

Config
  config        validate and print a config file, the resolved config, or the key reference
  check         validate the config and every app without starting anything
  rescan        re-read the apps directory, every dboss.yaml and the host defaults
  ports         show the live port table, one fixed port per app process
  password      print a bcrypt hash for basic_auth
```

`dboss start` always runs in the foreground; systemd is the daemonizer and `dboss systemd --install` writes and enables the unit.
Every other command talks to the running host over its control socket and accepts `--json`.
Inside an app folder the app argument defaults to that app.

### Startup and the running list

On start the host clears every listener in `ports.range`, then starts the apps listed in `state_dir/running.json`.
That file is written on every `run` and `stop`, so an app you stopped stays stopped across restarts.
When the file does not exist yet, which is the case on a first start, every discovered app is started.
A stopped app is also started by the first proxied request, which gets a "starting" page that refreshes after `proxy.wake.retry_after` seconds.

## Management console

The console is served for `management.host` on the proxy listener and again on the first port of `ports.range` (`3100` in the demo), where `127.0.0.1` is also accepted for `dboss login` sessions.
It shows every app with state, uptime, memory, last activity and request rate, offers start, restart, stop and maintenance controls, links to the process logs, and edits the host and app `dboss.yaml` files in place with validation, conflict detection and a "restart required" notice for host keys that only apply on the next start.

### Signing in

Production sign-in goes through AuthCog: the console redirects to `management.auth.realm`, and only the addresses in `admin_emails` are admitted.

For local work there is `dboss login`:

```
$ dboss login
http://127.0.0.1:3100/login?token=...
Opens the console as cli@localhost. Valid for 3 minutes, one use.
```

The link is minted by the running host over the control socket, so only someone with access to the socket can create one.
It works once, expires after 3 minutes, and signs the browser in as `cli@localhost` with the same signed session cookie AuthCog logins get.
AuthCog can never vouch for that address, so the two paths do not overlap.
`dboss login --json` prints `{"url": ...}`.

The link uses the console's own loopback listener, the first port of `ports.range`, so it needs no DNS.
That listener accepts `127.0.0.1` and `localhost` only for sessions created this way; without one it shows a page telling you to run `dboss login`.
From another machine, tunnel the port first: `ssh -L 3100:127.0.0.1:3100 <host>`.

### Frontend

The console is a [fez](https://github.com/dux/fez) application.
Everything lives under `./internal/console/static/` and is embedded in the binary:

* `index.html` - the SVG icon sprite and a single `<db-shell>` tag, plus one `<script fez="...">` tag per component.
* `fez.min.js` - the fez runtime, copied from https://dux.github.io/fez/dist/fez.min.js.
* `fez/db-shell.fez` - navbar, section tabs, hash-routed views, API calls, the 5 second poll; exposed as `Boss`.
* `fez/db-overview.fez` - stat cards and the service list.
* `fez/db-app-card.fez` - one service: status badge, datagrid, process table, actions.
* `fez/db-config.fez` - config file list and editor.
* `fez/db-config-keys.fez` - searchable key reference shown in the drawer by the Help button.
* `fez/db-toast.fez` and `fez/db-drawer.fez` - self-mounting singletons exposed as `Toast` and `Drawer`.
* `app.css` - the whole stylesheet, a light Tabler-style theme; components carry no `<style>` blocks.

There is no build step: fez compiles the components in the browser.
The console's Content Security Policy allows `'unsafe-inline'` and `'unsafe-eval'` for scripts and `'unsafe-inline'` for styles because fez needs them; every origin other than the console itself stays blocked, so nothing loads from a CDN.

## Layout

```
cmd/dboss/            entry point
internal/cli/         commands, help text, systemd unit, host session wiring
internal/config/      dboss.yaml model, validation, embedded reference.yaml
internal/apps/        app discovery and the config file store the console edits
internal/super/       process supervisor, health checks, idle stop, state files
internal/ports/       fixed port allocation inside ports.range
internal/proxy/       host routing, static files, maintenance, wake, request logging
internal/reqlog/      per-app SQLite request logs and rates
internal/console/     management console: auth, JSON API, embedded fez frontend
internal/ctl/         control socket protocol, server and client
internal/res/         process placement (process groups)
web/                  starting, crashed, maintenance and 404 pages
demo/                 host config and two sample apps
doc/                  design documents
```

## Validation

```sh
make check                                   # go vet + go test ./...
go test ./internal/console/                  # console API and auth, including dboss login
bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'   # component syntax check
```
