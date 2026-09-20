# dboss

Bare-metal app host for one Linux box, in a single Go binary called `dboss`.
It runs the processes described by each app's `dboss.yaml`, hands every process a fixed `PORT`, proxies HTTP to the right app by hostname, stops idle apps and wakes them on the next request, and ingests every process log and request row into a per-app SQLite log store.
A built-in management console shows live state, controls the supervisor, edits the config files on disk and searches the logs.
Daemon features are modules with a common lifecycle, so a new one (an ingestion sink, a security filter) plugs in at one place.

It sits directly behind Cloudflare as the origin.
There is no TLS, no containers and no deploy logic; rsync, releases and rollback stay in lux-deploy, which calls `dboss` at the end of a deploy.
The configuration reference ships in the binary: `dboss config --reference`, also embedded from `./internal/config/reference.yaml`.

## Requirements

* Go 1.25 or newer to build.
* Linux for production, macOS for development.
* Nothing else at runtime: the binary embeds the console assets and the configuration reference.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh
```

The installer asks GitHub for the latest release, downloads the `linux` or `darwin` binary for the machine's architecture (`amd64` or `arm64`), verifies it against the release checksums and installs it as `/usr/local/bin/dboss`.
Set `DBOSS_INSTALL_DIR` to change the target, or `DBOSS_VERSION` (for example `v0.1.0`) to pin a release:

```sh
DBOSS_INSTALL_DIR=$HOME/bin DBOSS_VERSION=v0.1.0 \
  curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh
```

Releases are built by `.github/workflows/release.yml` on every `v*` tag push; building from source is still the option below and needs Go 1.25+.

## Build and run the demo

```sh
make build            # ./bin/dboss
make demo             # builds, then runs the host session on ./demo/dboss.yaml
```

The demo listens on `:80` and hosts three apps.
Binding port 80 needs root or `CAP_NET_BIND_SERVICE`, so `make demo` runs the daemon through `sudo`.

* http://dboss.lvh.me - management console
* http://sinatra.lvh.me - Ruby app (`autostart: false`, wakes on first request; needs the Ruby from `./demo/apps/sinatra/mise.toml` and `bundle install`)
* http://bun.lvh.me - Bun app
* http://button.lvh.me - Bun app with `autostart: button`; it serves a start button and only its POST brings it up, so a crawler or favicon request never starts it (stop it in the console to see the page again)

`make demo-watch` rebuilds and restarts on source changes through `watchexec`.
`make kill` stops the demo apps and clears the port range after a crash.

## One config file, two modes

`dboss.yaml` is the only configuration file.
A file with `procfile` describes an app; any other file describes a host that runs a directory of apps (default `./apps`).
`dboss.local.yaml` next to it wins when it exists and is meant for server-only overrides (gitignored).
Every command looks for the config as `-c path`, then `$DBOSS_CONFIG`, then the current folder.

Every host key has a sane default - `apps: ./apps`, `proxy.listen: ":80"`, `ports.range: [3100, 3990]`, the runtime paths under `./.dboss`, the AuthCog realm, session lifetime, metrics, upstream timeouts, wake pages and the log cadence - so a host file only names what deviates. With no config file at all, `dboss start` runs the default host: `:80`, `./apps`, console off.

Host file (`./demo/dboss.yaml`):

```yaml
management:
  host: dboss.lvh.me
  url: http://dboss.lvh.me   # optional; defaults to https://<host>
  auth:
    admin_emails:
      - you@example.com

ports:
  range: [3100, 3199]

defaults:
  idle_stop: 0s
```

App file (`./demo/apps/bun/dboss.yaml`):

```yaml
procfile:
  web:
    command: ./start.sh
    domains: [bun.lvh.me]
    health: /up
```

The proxy listens on `:80` by default and owns that port for every app; the demo uses the same address, so a hand-run session needs root or `CAP_NET_BIND_SERVICE`.
Every process that declares `domains` is a web process, and an app may have several, each serving its own hostnames; a process with only a command is a background worker. Running dboss inside an app folder with no domains binds the first process to `.lvh.me`.
Every key that takes a list also accepts a single value, so `allow_ips: 10.0.0.0/8` equals `allow_ips: [10.0.0.0/8]`.
A leading `*.` in a domain matches subdomains only; a leading `.` matches the bare domain and every subdomain, so `domains: .myapp.com` covers `myapp.com` and `*.myapp.com`.
The web process can also set `canonical_host` (one of its domains); every other domain answers 301 to it, so `www` never serves content.
`proxy.listen` and `management.host` are such lists: several listen addresses each get a listener with the same routing, and several console hostnames are all accepted.
A `$NAME` in a value is replaced with that variable from the daemon's environment at load time, so `url: $ALERT_WEBHOOK_URL` keeps a secret out of the file; only all-uppercase names expand, an unset name stays as written, and `procfile` and cron commands are never expanded because they are runtime shell lines.
Every app-level key can be set once under `defaults:` in the host file and repeated at the top level of an app file; the app value wins key by key.
`dboss config --keys [filter]` lists every key grouped by block, with a one-line description, its default and, when useful, an example; the same list is behind the Help button in the console's Configuration view.
`dboss config --reference` prints the long annotated reference, and `dboss config [app] -d` prints a resolved config with every default filled in.
`dboss init` prints a fully commented starter config, service or app, with every key shown with its default or an example; save it with `dboss init > dboss.yaml`.

```
$ dboss config --keys health
Runtime  (defaults: in the root file, top level in an app file; per-process ones also under processes.<name>)
  health_interval      poll interval of the readiness and liveness checks                                                  500ms  per process
  health_timeout       give-up time of the readiness check; counts as a failed restart                                     1m     per process
  unhealthy_threshold  consecutive liveness failures of the web process before it is restarted; 0 disables ongoing checks  3      per process
```

## Commands

```
dboss <command> [options]
dboss help <command>

Host session
  start         run the host session in the foreground; Ctrl-C stops every app
  systemd       print the systemd unit for this config, or install and enable it
  kill          stop every app and terminate every listener left in ports.range
  login         print one-time console URLs that sign you in as cli@localhost

Apps
  ls            list apps with state, ports, uptime, last activity and memory
  run           start an app; rescans first when it is not known yet
  stop          stop an app and keep it stopped until run or the next request
  restart       stop and start an app on the same ports
  destroy       stop and permanently remove an app with deletable: true
  status        full detail for one app: processes, restarts, resources, request rates
  logs          print or follow the process logs of an app
  maintenance   answer every request with the maintenance page while the app keeps running
  cron          list an app's scheduled jobs, or run one now
  hooks         list an app's deploy hooks, run one, or rotate its secret
  exec          run a one-off command in the app's environment
  audit         list operator actions: start, stop, restart, destroy, hook runs and config writes

Config
  init          print a fully commented starter config for a service or an app
  config        print a config file, the resolved config, the key reference, or saved revisions
  check         validate the config and every app without starting anything
  doctor        preflight a box: tools, writable dirs, valid config and a clear port range
  rescan        re-read the apps directory, every dboss.yaml and the host defaults
  ports         show the live port table, one fixed port per app process
  password      print a bcrypt hash for basic_auth
  sshkey        list the local SSH public keys, or create a new key
```

`dboss start` always runs in the foreground; systemd is the daemonizer and `dboss systemd --install` writes and enables the unit.
Every other command talks to the running host over its control socket and accepts `--json`.
Inside an app folder the app argument defaults to that app.

### Startup and the running list

On start the host clears every listener in `ports.range`, then starts the apps listed in `state_dir/running.json` that have `autostart: true` (the default).
That file is written on every `run` and `stop`, so an app you stopped stays stopped across restarts.
When the file does not exist yet, which is the case on a first start, every discovered app with `autostart: true` is started.
An app with `autostart: false` stays down across host restarts until `dboss run`, the console, or the first proxied request starts it.
An app with `autostart: button` also stays down, but a request answers a page with a start button and only its POST starts the app, so a crawler or a favicon request never does.
A stopped app is also started by the first proxied request, which gets a "starting" page that refreshes after `proxy.wake.retry_after` seconds.

## Logs

Every app has one SQLite database at `log_dir/<app>/dboss.sqlite` with three tables:
`requests` (one row per proxied request, written by the proxy), `logs` (one row per log line,
written by the ingestion module) and `tail_offsets` (how far the file tailer has read).
`logs` carries `ts`, `source`, `process`, `stream`, `level`, `message`, `request_id` and `raw`,
and an FTS5 index over `message` and `raw` backs the text search.
`requests` carries `request_id` (the `CF-Ray` when Cloudflare sent one, so a request from the Cloudflare dashboard can be found by pasting its Ray ID into the search) and `country` (the two-character `CF-IPCountry`, empty without it).

Each row belongs to a channel and the console's **Logs** viewer selects one:

* `REQUEST` - the proxy's request rows.
* `STDOUT` - the stdout/stderr of each app process, sealed and parsed by the ingestion module.
* `dboss` - dboss's own daemon log, mirrored into the reserved `log_dir/_dboss` database and
  offered as **Host (dboss)** in the app picker.
* one channel per `*.log` file the app writes under `<app dir>/log`, tailed by byte offset and
  never rotated or deleted.

`REQUEST` rows and app log files are kept for `log_retention` (default `336h`, two weeks);
`STDOUT` and the dboss daemon log for `stdout_retention` (default `3h`). Both are deleted by the
daily prune; `log_retention: 0` disables the store for the app.
A second daily job at `daemon.vacuum_at` (default `04:30`) runs SQLite `VACUUM` on every app database and the host database to reclaim the freed space, including databases left behind by apps removed from the config; set it to `""` to disable.
The supervisor owns the process log file: every `daemon.log_ingest_interval` (default `5s`) it
seals the current segment into `<process>.log.<unix>.sealed` and opens a fresh one, then the
ingestion module parses the sealed segment, commits its rows to the database and only then deletes the file, so a transient database error cannot lose lines.
A segment whose commit failed stays on disk and is picked up again by the next pass.
A JSON line is read for `level`, `message` and `request_id`; any other line keeps its text and a
keyword guess for the level.

One row is one record, not one physical line:

* A line that starts with a space or a tab belongs to the line above it, so a stack trace or an indented dump is a single row.
* A Rails `Started GET "/path" ...` line opens a request row that runs to its `Completed <status>` line; a `5xx` status makes the row `error`, a `4xx` makes it `warn`.
* A leading `[<request id>]` tag (Rails `config.log_tags = [:request_id]`) fills `request_id` and is removed from the message. dboss sends the id as `X-Request-ID`, the same one stored on the `REQUEST` row. Lines only join a row with the same id, so concurrent requests split into more rows instead of mixing.
* Blank lines are dropped and ANSI colors are stripped from the message; the expanded row still shows the lines as written in `raw`.
* A row is capped at 1000 lines or 256 KiB. Its level comes from its first line.

A record that is still being written is not cut: while a log was written to in the last 2 seconds its last open row waits for the next pass.
`dboss logs -f` still tails the live file, while `dboss logs --search q [--level l] [--channel c] [-n rows]` queries the same store the viewer uses and prints matching rows.

The full-screen viewer at `/logs` (the **Logs** button on an app card, opened in a new window)
filters by channel, time range, level or HTTP method/status and free text, highlights matches,
expands a row to its raw fields and exports the current query as text.
The current filters live in the URL query string, so a view can be bookmarked or shared.
It is a second fez page (`log.html`), independent of the console shell.

## Scheduled jobs

An app declares one-shot commands the daemon runs on a schedule, independent of whether the app itself is running:

```yaml
cron:
  cleanup:
    schedule: every 6h          # or "0 7 * * 1-5"
    command: bundle exec rake cleanup
    timeout: 30m                # optional; a run over it is killed
    overlap: false              # optional; false skips a run while the previous one goes
```

`schedule` is either `every <n><s|m|h|d>` or a standard five-field cron expression.
Jobs run in the app folder with the app environment, log to their own `cron-<job>` channel in the log store, and do not count as activity for idle stop.
A stopped or idle app still fires its jobs, and there is no catch-up after a daemon restart.
`dboss cron [app]` lists jobs, next run and last result; `dboss cron run [app] <job>` starts one now; the console card has a **Run** button.

## Deploy hooks

An app can declare one-shot commands a signed HTTP ping triggers, so a Git host webhook can start a deploy without any shell access:

```yaml
hooks:
  deploy:
    command: git -C .. pull --ff-only
    timeout: 10m
    restart: true      # restart the app when the command exits 0
    overlap: false      # skip a ping while the previous run is still going
    # secret: $DEPLOY_HOOK_SECRET
```

The ping URL is `https://<management.url>/hooks/<app>/<hook>`. Authentication is a token, accepted as `?token=<secret>` in the URL (paste the whole URL into GitHub), `Authorization: Bearer`, `X-Gitlab-Token`, or a GitHub `X-Hub-Signature-256` HMAC over the raw body. `X-GitHub-Event: ping` (sent when the webhook is created) is acknowledged without running anything.

With no `secret` in the config, dboss generates a 64-character secret under `state_dir/hook-secrets.json` on first use and never writes it to the config. `dboss hooks [app]` lists hooks with their last result and the ready-made ping URL; `dboss hooks run [app] <hook>` starts one now; `dboss hooks rotate [app] <hook>` mints a new secret, invalidating the old URL. Hooks run in the app folder with the app environment, log to a `hook-<name>` channel, and leave the app alone unless `restart: true`.

`dboss exec [app] <command> [args...]` runs a one-off command in the same environment and prints its combined output. Options come before the command, so the command's own flags pass through; `--timeout` (default 1m) kills it, and its exit code becomes dboss's exit code.

## PubSub channels

A web process can serve a pub/sub hub on its domains. Set `pubsub` and dboss answers the path instead of forwarding, so subscribers connect even while the app is stopped and realtime traffic never wakes it. `pubsub: true` uses `/socketio`, `pubsub: /path` sets a custom prefix, and a mapping sets the full options:

```yaml
procfile:
  web:
    command: ./start.sh
    domains: [myapp.com]
    pubsub: true            # or /socketio, or a mapping
    # pubsub:
    #   path: /socketio
    #   secret: $PUBSUB_SECRET   # bearer for HTTP publish; empty generates one per web process under state_dir
    #   replay: 10               # messages kept per channel and replayed to a late subscriber
    #   max_clients: 500         # subscriber cap per hub; 0 means unlimited
    #   max_message_size: 64k    # largest publish body; 0 means unlimited
    #   client_events: true      # a WebSocket client may publish to its own channel
    #   test: false              # serve the browser self-test at <path>/_test
```

`pubsub` lives on a web process, because the hub is served on that process's domains. An app with several web processes may run one hub per process, each with its own path, secret and channels.

**Subscribe.** A `GET <path>/<channel>` upgrades to a WebSocket, or streams SSE when the request carries no `Upgrade` header. Messages are `{"event","data","ts"}`; the last `replay` are replayed to a subscriber that joins late, oldest first. A slow subscriber is dropped rather than blocking the publisher.

The bundled client, served at `GET <path>/client.js`, needs no dependency and picks WebSocket with an SSE fallback. It derives the path from the directory it was served from, so `Pubsub.connect()` needs no arguments:

```html
<script src="/socketio/client.js"></script>
<script>
  const chat = Pubsub.connect().channel('chat');
  chat.on('message', (envelope) => console.log(envelope.event, envelope.data));
  chat.on('open', () => chat.send('typing', { user: 'a' })); // WebSocket only
  chat.on('close', () => {});
  chat.on('error', (err) => {});
</script>
```

**Publish.** A `POST <path>/<channel>` with the app's secret publishes to every subscriber. A body shaped like `{"event","data"}` is sent as written; any other body becomes the data of a `message` event:

```bash
curl -X POST https://myapp.example.com/socketio/chat \
  -H "Authorization: Bearer $PUBSUB_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"event":"message","data":{"text":"hello"}}'
```

The secret is accepted as `?token=`, `Authorization: Bearer` or `X-Pubsub-Token`, and satisfies a publish even when the app sets `basic_auth`. With no `secret` in the config, dboss generates a 64-character one per web process under `state_dir/pubsub-secrets.json` on first use; `dboss pubsub` prints it, and `dboss pubsub rotate [app]` replaces it.

**Self-test.** With `test: true`, `GET <path>/_test` serves a page that opens a WebSocket and an SSE connection and reports PASS or FAIL in the browser.

`dboss pubsub [app]` lists channels and subscriber counts, `dboss pubsub secret [app] [--process name]` prints the credential and example URLs, `dboss pubsub publish [app] <channel> [--event name] [--data json|-] [--process name]` sends a message through the control socket, and `dboss pubsub help` prints the integration guide. Name `--process` only when the app runs several hubs. The console's **PubSub** tab (when any web process sets `pubsub`) shows the same and can publish a test message. Channels are one path segment; `client.js`, `_test` and `_selftest` are reserved. Metrics are `dboss_pubsub_clients`, `dboss_pubsub_channels` and `dboss_pubsub_messages_total`, each labeled by app.

## Health and metrics

The management host also serves three endpoints, enabled by `management.metrics.enabled` (default `true`):

* `GET /healthz` - `200 ok` while the daemon is up.
* `GET /readyz` - `200` only while every `autostart` app serves (running, or asleep and woken by the next request), else `503` with the apps that are not ready.
* `GET /metrics` - Prometheus text: build info, per-app up/state/uptime/memory/CPU, per-process restarts and memory, request rates per window, request duration quantiles (p50/p95/p99 over the last hour), and the last exit of each cron job and hook.

`healthz` and `readyz` are open so an uptime checker or load balancer can reach them. `metrics` is open too unless `management.metrics.token` is set, then it requires `Authorization: Bearer <token>`. All three answer on the management host only.

Each app also answers on its own hosts at `health_endpoint` (default `/.well-known/dboss/health`): `200 {"app","state"}` while a visitor would be served, `503` otherwise. An app stopped by `idle_stop` (or `dboss stop`) still answers `200` with `"state":"stopped"`, because the next request wakes it, so a Cloudflare Health Check or Load Balancer never flags a sleeping app. Draining, maintenance, starting, crashed and a stopped `autostart: button` app answer `503`. It runs before basic auth and never wakes a stopped app, so a Cloudflare health check or uptime monitor can probe the app domain directly. Set `health_endpoint: ""` to disable it.

The supervisor also watches each web process for its whole lifetime: the `health` path declared on the web procfile entry (e.g. `/up`, or omitted for a TCP connect) gates startup readiness within `health_timeout`, then the same check runs every `health_interval`; after `unhealthy_threshold` consecutive failures (default `3`) the process is killed and the normal restart policy, backoff and `max_restarts` apply. Set `unhealthy_threshold: 0` for startup-only readiness. Background workers are not polled.

`dboss doctor` preflights a box before a first start or a deploy: it checks that `lsof` is on `PATH`, that `state_dir`, `log_dir` and the socket directory are writable, that the config and every app load, and whether anything still listens in `ports.range` (a warning, since a start clears it).

## Notifications

A host can post runtime events to one operator webhook:

```yaml
notify:
  url: $ALERT_WEBHOOK_URL
  format: generic       # generic | slack | discord | ntfy
  events: [crash, restart-loop, health-timeout, wake-failed, hook-failed, deploy, config-changed, backup-failed, error-rate, slow]
  min_interval: 5m       # per app and event, so a crash loop does not spam
  headers: {}
```

`crash` is an app entering the crashed state, `restart-loop` a process failing again after a restart, `health-timeout` the readiness check giving up or the web process failing its liveness checks, `wake-failed` a request that could not start a stopped app, `hook-failed` a deploy hook that exited non-zero, `deploy` a `restart: true` hook that succeeded and rolled the app, `config-changed` a config write that changed a host key and needs a restart, `backup-failed` a PostgreSQL dump that failed, `error-rate` an app answering with too many 5xx, and `slow` an app whose p95 latency crossed its limit (both from the app's `alerts:` block, checked once a minute over `alerts.window`, default `error_rate: 10` percent and `slow_p95` off). Sends are queued and best-effort, so a slow or dead endpoint never blocks the supervisor; `min_interval` debounces repeats. The delivered/failed/dropped counts are exported as `dboss_notifications_total`. `url: ""` (the default) disables notifications.

## PostgreSQL inspection and backups

When a PostgreSQL server is reachable, the console gains a **PostgreSQL** tab and the daemon can back up selected databases on a daily run.

Configuration is one host-level block. The only backup setting is the per-database rotation window:

```yaml
postgres:
  enabled: true
  dsn: $DATABASE_URL            # empty auto-detects the local socket, then 127.0.0.1:5432
  backup:
    databases:
      myapp_production:
        rotation: week          # keep 7 days; month keeps 30
      reports:
        rotation: month
```

The connection resolves in order: `postgres.dsn` when set, then a unix socket (`/var/run/postgresql`, then `/tmp` for Postgres.app), then `127.0.0.1:5432`, with the libpq `PG*` environment merged in. A daemon started with `sudo` runs as `root`, whose matching Postgres role does not exist, so detection impersonates the invoking `SUDO_USER`; set `postgres.dsn` explicitly when the service user has no matching role. The tab shows the server version, uptime, connection count, cache hit ratio, WAL LSN, replication state, live activity including the longest query and lock waits, and every database with its size, owner and last backup.

Backups are per-database logical dumps (`pg_dump --format=plain`) zipped as `pg_backup/<database>/BACKUP_<timestamp>.zip` next to the apps. One run happens each day at 04:00 UTC; scheduled dumps older than the database's rotation window (7 days for `week`, 30 for `month`) are pruned from disk and the catalog, while a manual **Back up now** is kept. The catalog lives at `state_dir/pg-backups.json`.

The console writes the selection to `dboss.local.yaml` (the host override is created from the base when missing) and hot-reloads the daemon, so no restart is needed. A plain edit on disk applies on the next config save or `dboss rescan`. Restore verifies the archive and loads into a **new** database named `<source>_restore_<timestamp>` by default; replacing an existing database requires an explicit target and confirmation. The per-database panel also has **Drop database**, which needs the database name typed as confirmation.

On the box this feature needs `pg_dump` and `psql` on the service user's `PATH`, and a role that can read every selected database (`pg_read_all_data` or ownership). The CLI mirrors the tab:

```bash
dboss pg                      # server summary and databases
dboss pg backups              # recorded dumps
dboss pg backup [database]    # dump one or every selected database
dboss pg delete <backup-id>   # remove one recorded dump
dboss pg restore <id> [--target name] [--force]
dboss pg drop <database> --confirm <database>
```

The metrics endpoint exports `dboss_pg_up`, `dboss_pg_database_size_bytes`, `dboss_pg_backup_last_success_timestamp_seconds` and `dboss_pg_backup_count`.

## Containers

dboss supervises native processes and does not start or manage containers.
Run a Docker-packaged app with Docker Compose as its own system and let Cloudflare reach the container's published port directly; dboss stays out of that path.

Two rules keep the two systems from colliding:

* Keep container ports outside `ports.range`. On start dboss clears every listener in the range and before each spawn frees the app's fixed port, so a container listening there would be killed.
* A hostname is routed by one proxy only. dboss routes just the hosts of the apps in its own `apps` directory, so a container host must be served by Cloudflare or another reverse proxy.

The one bridge without code is a procfile wrapper (`shell: true` with `docker run -p 127.0.0.1:$PORT:$PORT ...`), which makes a container answer as a dboss app but leaves its lifecycle on the docker CLI, with the usual caveats around stopping it.

## Access control

`basic_auth` puts HTTP basic auth in front of the whole app, static files included.
It maps a user to a bcrypt hash printed by `dboss password`; set it under `defaults:` in the host file to protect every app on a staging box with one block.
`allow_ips` limits the app to a list of CIDRs, matched against the client IP from `proxy.client_ip_headers`.

```yaml
basic_auth:
  alice: "$2a$10$..."   # dboss password
allow_ips:
  - 10.0.0.0/8
```

`auth` puts an AuthCog sign-in in front of the app, the way Cloudflare Access does, for people instead of shared passwords.

```yaml
auth:
  allow_emails: [ana@example.com, "*@example.com"]   # empty leaves the app open
  session_ttl: 24h
```

A visitor without a session is sent to AuthCog (`management.auth.realm`), returns to `/.well-known/dboss/auth` and gets a signed, host-only cookie; `/.well-known/dboss/logout` signs out.
Only the listed emails and `*@domain` patterns get in, and the list is checked on every request, so removing an entry ends that session on the next `dboss rescan`.
The app receives the signed-in email as `X-Dboss-User`; dboss strips that header from every inbound request, so the app can trust it.
A request that does not accept `text/html` gets `401` instead of a redirect.
AuthCog returns over plain http only to a local host on a port above 999, so local testing needs a `proxy.listen` port such as `:8080`.
`basic_auth` and `auth` are independent: when both are set, both must pass.

`authcog` is the app-level login service: dboss runs the whole AuthCog round trip so the app needs no AuthCog code of its own.

```yaml
authcog:
  login: true      # dboss runs the sign-in for this app
  path: /authcog   # app URL dboss captures; must match the AuthCog realm redirect_path
  realm: auth      # auth.authcog.com
```

The app links to `path`. dboss mints the challenge, sends the browser to `https://<realm>.authcog.com/d:<host>`, and on the `?callback=` return exchanges the one-time hash server-side. It then forwards one request to the app's own `path` route with the profile in `X-Dboss-User` (`{"email","name","avatar","provider"}`). The app reads what it needs and creates its own session; dboss keeps no session. `X-Dboss-User` is removed from every inbound request, so only dboss can set it, and it is set only on that post-login request. Any AuthCog account is admitted, and logout is the app's job. `authcog` is independent of `auth`: its login path is never gated by `auth`.

Each request walks the stages in this order: canonical redirect, `allow_ips`, health endpoint, `authcog` login, `auth` sign-in, `basic_auth`, maintenance, static files, body buffer, then wake or forward.

* A request with no or wrong credentials gets `401` at the auth stage and never reaches the wake stage, so a crawler or scanner cannot start a protected sleeping app. The first request with valid credentials wakes it.
* With no `basic_auth` any request wakes a stopped app, except an `autostart: button` app, which only its start button's POST wakes.
* The health endpoint answers before auth, so a Cloudflare health check works on a protected app. It never wakes the app.
* A pubsub publisher presenting the app's publish secret passes without the basic-auth credentials or a sign-in session.
* The same holds for `auth`: no session means no wake, the health endpoint stays open, and static files are protected.

## Static files and error pages

Each web process serves `./public` straight from disk (`static` on the web procfile entry, relative to the app folder) for GET and HEAD, without waking the app.
Set `static: /path` for another directory or `static: false` to disable it for that process.
Only the common asset types in `static_extensions` are served: css, js, mjs, map, json, txt, xml, ico, images, fonts, mp4, webm, mp3, pdf, wasm and webmanifest.
A missing file, a directory, or a file with any other extension (an `.html` page, a dotfile, no extension) is a normal request to the app, so a route always wins over a stray file.
A missing `public` folder simply turns static serving off; `static_extensions: []` serves any regular file.
Paths under `static_immutable` (default `/assets/`) are cached as immutable for a year, everything else for an hour.

`error_page_path` names one static HTML file, relative to the app folder (for example `public/error_500.html`).
It is sent as it is on disk and read on every request, and only a GET that accepts `text/html` ever gets a page; API and non-GET requests are never rewritten.

* dboss's own errors (`502` when the app is unreachable, timed out or has no port) answer with this file, or with the built-in `./web/error.html` page when the key is empty or the file is unreadable.
* The app's own `5xx` answers are replaced with this file, status kept, only when the key is set and the file is readable. With the key empty an app keeps its own error page.

## Restarts and forwarded headers

`dboss stop` and `dboss restart` (and the console buttons) first mark the app **draining**: the proxy answers new requests with `503` and `Retry-After`, while requests already in flight finish, bounded by `stop_timeout`. Only then does the supervisor send `stop_signal` to the process group. The app card shows a `draining` badge and `dboss ls` prints it in the state.

`deletable: true` opts an app into permanent removal through `dboss destroy <app>` or the console's **Destroy** button; the default is false.
Destroy drains and stops the app, clears its running and maintenance state, removes its entry from the apps directory and drops it from the live host.
A plain app folder is removed recursively; an app symlink is unlinked without following its target, which remains owned by lux-deploy.
Single-app mode cannot destroy itself, and retained logs, audit rows and config history continue through their normal retention.

On the way to an app the proxy adds `X-Forwarded-Proto`, `X-Forwarded-Host` and `X-Real-IP` when they are missing; whatever Cloudflare sent is left untouched. `X-Forwarded-For` is appended by the reverse proxy.

An app's processes start with the web processes (the ones with `domains`) first, then the rest in name order, so a web process that expects other services to be up still gets that.

## Audit log

Every mutating action records who did what to which app and how it turned out: start, stop, restart, destroy, maintenance, rescan, cron runs, hook runs and rotations, `exec`, and config file writes and restores. Console actions carry the signed-in email, a hook ping carries `hook:<app>/<hook>`, and control-socket actions are attributed to `cli`.

Rows live in an `audit` table in the reserved `_dboss` database, are kept for `daemon.audit_retention` (default `8760h`, `0` keeps them forever), and are pruned with the daily log prune. The console has an **Audit** tab with app, actor and action filters; `dboss audit [--app name] [--actor who] [--action name] [-n rows]` prints the same rows.

## Config history

Every config save first copies the current file to `state_dir/config-history`, keeping the last 50 revisions per file. In the console's Configuration view the **History** button lists them; **View** shows a revision and **Restore** writes it back (revision-checked, then rescanned, and recorded in the audit log). From the CLI:

```
dboss config history [app]
dboss config restore [app] <revision>
```

Both work on the host file (no app) or one app's file. A CLI restore writes the file on disk; the running host applies it on the next `dboss rescan`.

## Management console

The console is served for `management.host` on the proxy listener and again on the first port of `ports.range` (`3100` in the demo), where `127.0.0.1` is also accepted for `dboss login` sessions.
`dboss start` prints the loopback address first, and the public address too (`management.url` when set, otherwise `https://` on the first `management.host`):

```
management console: http://127.0.0.1:3100 (run `dboss login` for a one-time sign-in link)
management console: https://dboss.example.com (AuthCog sign-in)
```
`dboss start --login` also prints a one-time loopback sign-in link on stdout (never in the daemon log); `make demo` uses it.
It shows every app with state, uptime, memory, last activity and request rate, offers start, restart, stop and maintenance controls, and adds a typed-confirmation destroy action when the app sets `deletable: true`.
It links to the process logs and edits the host and app `dboss.yaml` files in place with validation, conflict detection and a "restart required" notice for host keys that only apply on the next start.
The **Config** view has two modes: **YAML** edits the raw file, and **Form** offers a visual editor built from recipes (PubSub channels, Web, Health and runtime for an app; Notifications and the PostgreSQL connection for the host).
Each field shows a friendly label, its key, the description from the key reference and the default as a placeholder; a blank field means "use the default", so the key is removed from the file.
A form save is written to the server-only `dboss.local.yaml` next to the file (created from the base when missing), so a deploy never overwrites a value entered here.
The **Sys** tab is a read-only inspection of the box: hostname, OS and kernel, uptime, load, memory and disk use, the dboss runtime, chosen environment variables, and the installed toolchains (Go, Node, npm, Bun, Deno, Yarn, pnpm, Ruby, gem, Bundler, Python, pip, uv, PHP, Composer, Java, SQLite, lsof, rsync, curl, Docker, podman and more) with their paths and versions, each name linked to its project page.
It never starts, stops or changes anything; the `sysinfo` module keeps the snapshot warm and **Re-inspect** re-probes on demand.

### Signing in

Production sign-in goes through AuthCog: the console redirects to `management.auth.realm`, and only the addresses in `admin_emails` are admitted.
AuthCog returns over `http` only to a local host (`localhost`, `*.lvh.me`, an IP) on a port above 999, so a plain-http sign-in started on port 80 is routed through the console's own port (the first of `ports.range`) and then sent back to the address it started on.

For local work there is `dboss login`:

```
$ dboss login
local:  http://127.0.0.1:3100/login?token=...
public: https://dboss.example.com/login?token=...
Opens the console as cli@localhost. Valid for 3 minutes, one use.
```

The links are minted by the running host over the control socket, so only someone with access to the socket can create one.
They work once, expire after 3 minutes, and sign the browser in as `cli@localhost` with the same signed session cookie AuthCog logins get.
AuthCog can never vouch for that address, so the two paths do not overlap.
The loopback link and the public link carry the same single-use token, so opening one invalidates the other; run `dboss login` again for a fresh pair.
`dboss login --json` prints `{"url": ..., "public_url": ...}`.

The loopback link uses the console's own listener, the first port of `ports.range`, so it needs no DNS.
That listener accepts `127.0.0.1` and `localhost` only for sessions created this way; without one it shows a page telling you to run `dboss login`.
The public link goes through `management.host`, so it signs in from any browser that can reach the edge.
Without a public URL, tunnel the port first: `ssh -L 3100:127.0.0.1:3100 <host>`.

### Frontend

The console is a [fez](https://github.com/dux/fez) application.
Everything lives under `./internal/console/static/` and is embedded in the binary:

* `index.html` - the SVG icon sprite and a single `<db-shell>` tag, plus one `<script fez="...">` tag per component.
* `log.html` - the standalone full-screen log viewer page, a second `<db-log-shell>` entry point.
* `fez.min.js` - the fez runtime, copied from https://dux.github.io/fez/dist/fez.min.js.
* `fez/db-shell.fez` - navbar, section tabs, hash-routed views, API calls, the 5 second poll; exposed as `Dboss`.
* `fez/db-overview.fez` - stat cards and the service list.
* `fez/db-log-view.fez` - the log viewer: left nav of apps with sqlite size and fold-out channels, time range, filters, search, row detail and export; shared by the tab and the full-screen page.
* `fez/db-logs.fez` - the in-console Logs tab, a thin wrapper around `db-log-view`.
* `fez/db-log-shell.fez` - the full-screen page shell; exposes `Dboss` for `log.html`.
* `fez/db-app-card.fez` - one service: status badge, stats datagrid, actions.
* `fez/db-config.fez` - config file list, the YAML/Form mode toggle, the editor and revision history.
* `fez/db-config-form.fez` - the visual config editor: one form per recipe, driven by `config.Recipes()`.
* `fez/db-config-keys.fez` - searchable key reference shown in the drawer by the Help button.
* `fez/db-traffic.fez` - the Traffic tab: per-app requests over time, error rate, latency quantiles and the top paths, status codes, countries, client IPs and methods from the request log.
* `fez/db-audit.fez` - the Audit tab: operator actions with app, actor and action filters.
* `fez/db-sys.fez` - the Sys tab: read-only host facts, resource use and installed toolchains with versions.
* `fez/db-help.fez` - the Help tab: a topic list with the operator guide and the live key reference.
* `fez/db-toast.fez` and `fez/db-drawer.fez` - self-mounting singletons exposed as `Toast` and `Drawer`.
* `app.css` - the whole stylesheet, a light Tabler-style theme; components carry no `<style>` blocks.

There is no build step: fez compiles the components in the browser.
The console's Content Security Policy allows `'unsafe-inline'` and `'unsafe-eval'` for scripts and `'unsafe-inline'` for styles because fez needs them; every origin other than the console itself stays blocked, so nothing loads from a CDN.

## Layout

```
cmd/dboss/            entry point
internal/cli/         commands, help text, systemd unit, host session wiring
internal/daemon/      one host session: supervisor, modules, proxy, console, control socket
internal/module/      module lifecycle (start in order, close in reverse)
internal/config/      dboss.yaml model, validation, embedded reference.yaml
internal/apps/        app discovery and the config file store the console edits
internal/hook/        generated deploy-hook secrets under state_dir
internal/super/       process supervisor, health checks, idle stop, state files, log writer/seal
internal/ports/       fixed port allocation inside ports.range
internal/proxy/       filter pipeline, host routing, static files, maintenance, wake, request log
internal/logstore/    per-app SQLite log store: requests, channels, FTS search, tail offsets, prune
internal/ingest/      seals stdout, tails app log files and the dboss daemon log into the store
internal/logx/        leveled logger for dboss's own output (daemon.log_level)
internal/sysinfo/     read-only host inspection: OS, load, memory, disks and installed toolchains
internal/pg/          PostgreSQL inspection, scheduled dumps, retention and restore
internal/metrics/     Prometheus text rendered from the app snapshots
internal/notify/      debounced operator webhook for crash and failure events
internal/version/     release version, overridden at build time
internal/console/     management console: auth, JSON API, embedded fez frontend
internal/ctl/         control socket protocol, server and client
internal/ops/         one implementation of every app action, shared by CLI and console
internal/res/         resource backend: process groups or cgroup v2 limits
web/                  starting, crashed, button, maintenance, error and 404 pages
demo/                 host config and three sample apps
```

## Validation

```sh
make check                                   # go vet + staticcheck + go test ./...
go test ./internal/console/                  # console API and auth, including dboss login
bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'   # component syntax check
```
