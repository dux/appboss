# app-boss agent notes

Single Go binary (`appboss`) that supervises, proxies and logs the apps on one host.
Read `./README.md` for usage and the embedded configuration reference (`./internal/config/reference.yaml`, printed by `appboss config --reference`) before changing behavior.

## Deploy and configure

* Build once, then copy the binary to the box: `make build` produces `./bin/appboss`; install it at a stable path and pass that path to `appboss systemd --bin`. `~/bin/appboss` is only the local dev symlink and must not be used in a unit.
* Runtime deps on the box: Linux, `lsof` (start clears every listener in `ports.range`, `appboss kill` needs it too), a writable `state_dir`, `log_dir` and the socket directory. Go is build-time only.
* Host config lives at `<dir>/appboss.yaml`: `apps` (or `procfile` for single mode), `proxy.listen`, `ports.range` and `management.host` + `management.auth` are the minimum for a real host. Validate with `appboss check -c <dir>/appboss.yaml` before starting.
* Server-only values (admin emails, secrets, absolute paths) go in `<dir>/appboss.local.yaml`. It wins over `appboss.yaml`, is gitignored, and is never touched by a deploy, so it is how box edits survive. The console's "Create server override" copies `appboss.yaml` to it.
* Install and enable the service: `sudo appboss systemd -c <dir>/appboss.yaml --user <svc> --bin <path> --install` writes `/etc/systemd/system/appboss.service`, reloads and enables it. The unit runs `appboss start` as `<svc>` from the config dir with `Restart=always`, `RuntimeDirectory=appboss`, `AmbientCapabilities=CAP_NET_BIND_SERVICE` (port 80 without root) and `LimitNOFILE=65536`.
* Deploys are lux-deploy's job; app-boss has no deploy logic. After the release symlink swap it calls `appboss restart <app>` (the socket is `/run/appboss/appboss.sock`). `appboss check` is the pre-deploy gate.
* App config is `<app>/appboss.yaml`: `procfile` + `hosts` are the minimum. The console edits these same files on disk.
* Config lookup: `-c path`, then `$APPBOSS_CONFIG`, then `./appboss.local.yaml` or `./appboss.yaml`. Socket lookup: `--socket`, `$APPBOSS_SOCKET`, the config's `socket` when it exists, then `/run/appboss/appboss.sock`.
* One file name, two roles: a file with `apps:` is a host, a file with `procfile:` is an app, never both. `defaults:` in the host file applies to every app; an app's top-level key overrides it key by key, and process keys can be overridden once more under `processes.<name>`.
* Values take `$NAME` from the daemon environment at load (uppercase only, unset names stay literal); `procfile` and cron commands are never expanded. In `hosts`, a leading `*.` matches subdomains only and a leading `.` matches the apex and its subdomains.
* Boss injects `PORT`, `APP_NAME`, `PROC_TYPE` and `APPBOSS_SOCKET` into every process; `PORT` is one fixed value per (app, proctype) from `ports.range` and is never configurable. `appboss password` prints a bcrypt hash for `basic_auth`.
* Keys: `appboss config --keys [filter]` (one-line docs + defaults), `appboss config --reference` (long form, also `./internal/config/reference.yaml`), `appboss config [app] -d` (resolved config).
* Cloudflare is the edge: TLS, compression, caching, WAF and rate limiting stay there. Point `proxy.trusted_cidrs` at the Cloudflare ranges and keep `proxy.client_ip_headers: [CF-Connecting-IP, X-Forwarded-For]` so the origin cannot be reached directly.
* Management console is served on `management.host` and on `127.0.0.1:<first port of ports.range>`. Production sign-in is AuthCog (`management.auth.realm`, `admin_emails`); `appboss login` mints a one-time loopback URL for local access.
* App-level changes apply on `appboss rescan`; `apps`, `proxy`, `management`, `ports`, `daemon` and `state_dir`/`log_dir`/`socket` need a daemon restart.

## Working rules

* Build with `make build`; `~/bin/appboss` is a symlink to `./bin/appboss`, so a rebuild is what the shell runs.
* Validate with `make check` (vet and tests). Add a test next to the package you change.
* The demo host is `./demo/appboss.yaml`; run it with `make demo` or `appboss start` inside `./demo`. It listens on `:80`, so every hostname is port 80 and the daemon needs root (`make demo` uses `sudo`).
* Console static assets are embedded with `go:embed`. A CSS or component change needs a rebuild and a daemon restart to show up.
* Do not add Docker, TLS, rate limiting or deploy logic. Cloudflare owns the edge, lux-deploy owns releases.
* Config is real YAML on disk. Never introduce a database copy of the config.
* `appboss.yaml` is the only config file name; `appboss.local.yaml` is the server-only override and is gitignored.
* A new config key needs a description (and an example when it has no default) in `keyDocs` in `./internal/config/keys.go`; the test fails otherwise. Path, type and default are read from the structs and `Default()`. Update `reference.yaml` for the long-form text.
* State files under `state_dir` (`running.json`, `maintenance.json`, `last_activity.json`) are written by the daemon only.

## Modules and the request pipeline

* `./internal/daemon` is the only place a session is assembled: supervisor, `module.Manager`, proxy, console and control socket. `cli.start` loads the config and calls `daemon.Build`/`Run`. Register a new module there, not in the CLI.
* A long-running feature implements `module.Module` (`Name`, `Start`, `Close`) in `./internal/module`. `logstore.Store`, `./internal/ingest`, `./internal/sysinfo`, `./internal/pg` and `./internal/pubsub` are the current modules.
* `./internal/sysinfo` is the read-only host inspector behind the console's Sys tab: OS, kernel, load, memory, disks and a fixed toolchain probe list. `daemon.Build` registers its `Module` and hands `Inspector()` to `console.New` as a `SysReader`. It never mutates appboss state and writes no audit row; add a tool by adding a `probe` to `defaultProbes`.
* `./internal/pubsub` is the realtime channel hub behind `pubsub.*`. It is served by `Service.Filter`, registered as a proxy extra stage; `Service.AuthorizesPublish` lets a publish secret satisfy the proxy's `authorize`. It reuses the shared `config.Pubsub` on `Web` (so it rides `Snapshot.Web`), keeps one hub per `(app, channel)`, and stores generated secrets in `state_dir/pubsub-secrets.json`. The browser client and self-test are embedded (`client.js`, `test.html`); the integration guide is `pubsub.Help`, shared by `appboss pubsub help` and the console Help tab. Reserved segments are `client.js`, `_test`, `_selftest`. Long-lived subscribe requests call `SuppressRecord` so a socket's lifetime never skews request latency.
* The proxy pipeline is an ordered `[]proxy.Filter` built in `proxy.initFilters`. A new request filter is a function of that shape, inserted before the forward stage; the built-ins live in `./internal/proxy/filter.go`.
* `Manager.Stop`/`Restart` drain first: `requestDrain` sets the snapshot's `Draining`, new requests get 503, and `Manager.drain` waits on the per-app in-flight counter (`Manager.Enter`/`Leave`) up to the host `stop_timeout`. Draining runs off the app goroutine, so snapshots stay responsive.
* `forward` adds `X-Forwarded-Proto`/`X-Forwarded-Host`/`X-Real-IP` only when missing, so Cloudflare's values win. `startOrder` spawns `web_process` first, then the rest by name; `assignPorts` keeps its own name order.
* `./internal/res` is the resource backend seam. `Procgroup` (macOS and non-cgroup Linux) sums `ps` and ignores limits; `Cgroup` gives each (app, process) its own cgroup v2 directory under `/sys/fs/cgroup/boss`, writes `memory.max`/`cpu.max` and reads `memory.current`/`cpu.stat`. `selectCgroup` picks it when the hierarchy is writable and `resources` is not `procgroup`; a limit set while on procgroup logs a warning. `processEnv` in `./internal/super/supervisor.go` assembles the process environment in the documented priority order (daemon+mise, config env, `.env`/`.env.local`, injected).
* Actions reachable from both the CLI and the console belong on `ops.Service` (`./internal/ops`), not in either transport.

## Health and metrics

* The management host serves `/healthz`, `/readyz` and `/metrics` from `./internal/console/console.go` before the session auth. `management.metrics.enabled` (default true) turns them off; `management.metrics.token` gates only `/metrics`.
* `./internal/metrics` renders Prometheus text from `ops.Service.Apps()` snapshots, so metrics and the console can never disagree. Add a metric there, not in the handler. Request latency quantiles come from `logstore.Latency` through `ops.Service.Latency`.
* `Web.HealthEndpoint` (default `/.well-known/appboss/health`) is answered by the `publicHealth` filter in `./internal/proxy/filter.go` before auth: 200 while running and not draining, 503 otherwise, never wakes the app. `appboss doctor` uses `super.ListenersInRange`.
* The web process is monitored for its whole lifetime by `appRuntime.monitor` in `./internal/super/supervisor.go`: `health`/`health_interval`/`health_timeout` gate readiness, then `unhealthy_threshold` consecutive liveness failures emit `health-failed`, which kills the process so the normal exit path restarts it under `restart`/backoff/`max_restarts`. `0` disables liveness; workers are not polled.
* `./internal/version.Version` is the release version; the release workflow does not inject it yet, so `String()` falls back to the module version or the short VCS revision.

## Audit and config history

* `ops.Service.Do` writes one `audit` row per mutating action through the `Auditor` the log store implements (a type assertion in `ops.New`; a store without it disables auditing). Transports set `Request.Actor`: console = session email, hook ping = `hook:<app>/<hook>`, control socket = `cli`. Actions that bypass `Do` (config writes) call `Service.Audit` directly.
* Audit rows live in the reserved `_appboss` database (`audit` table), pruned by `daemon.audit_retention` in `logstore.Prune`.
* `apps.Store.Write` snapshots the current file to `state_dir/config-history` first, keeping 50 per file; `History`/`HistoryContents`/`Restore` back the console History panel and `appboss config history|restore`. Restore is a normal revision-checked `Write`.

## Notifications

* `notify:` in the host config points at one operator webhook. `./internal/notify` debounces per app and event, queues with a bounded buffer and posts best-effort, so it can never block the supervisor.
* `daemon.Build` creates the notifier and passes it to `super.New` (as a `notify.Sink`) and to `console.New` for its `appboss_notifications_total` counters. `Manager.Wake` is the proxy's start path and the only source of `wake-failed`; explicit run/console starts do not notify.
* Events are emitted by `appRuntime.emit` in `./internal/super`: `crash`, `restart-loop`, `health-timeout`, `hook-failed` and `deploy` (a `restart: true` hook succeeded). `ops.Service.Notify` covers `config-changed` from the console. A new event needs a name in `notifyEvents` in `./internal/config/config.go` and a line in the reference.

## Log store

* One SQLite database per app at `log_dir/<app>/appboss.sqlite`: `requests`, `logs` and the `logs_fts` FTS5 index, plus `tail_offsets` for the file tailer. `./internal/logstore` owns the schema, batching, search and prune.
* `logs.source` is the channel: `stdout` (process output), `appboss` (appboss's own daemon log, in the reserved `_appboss` database), or `file` with `logs.process` holding the app log path. `log_retention` prunes requests and `file` rows; `stdout_retention` prunes `stdout`/`appboss`.
* The supervisor writes through `super.logWriter`, which owns the file, rotates by size and can `Seal` a segment; `Manager.SealLogs` exposes it. Do not rename a live process log from outside the supervisor.
* `./internal/ingest` seals stdout segments and tails every `*.log` under `<app dir>/log` on `daemon.log_ingest_interval`. App log files are app-owned: track offsets, never delete them. Parse changes belong in `ingest.ParseLine`. It commits through `logstore.AppendLogs` (synchronous) and only deletes a sealed segment or advances an offset after that commit succeeds, so a transient database error cannot lose rows or duplicate reads.
* `log_retention` (default `336h`) covers requests and app log files; `stdout_retention` (default `3h`) covers stdout and the appboss daemon log; `log_retention: 0` disables the whole store for the app. The `appboss.sqlite` name is fixed; there is no config key for it.
* `daemon.vacuum_at` (default `04:30`) runs SQLite `VACUUM` on every app database and the host database through `Store.Vacuum`; empty disables it. It is separate from the prune loop.
* Prune and vacuum walk every database on disk (`Store.diskApps`), not just the current snapshots, so a database left behind by a removed app is still bounded. A failed insert keeps its batch for the next flush, capped; `AppendLogs` is the synchronous path for rows that must not be re-read.

## PostgreSQL

* `./internal/pg` owns the read-only inspector and the backup engine behind the console's PostgreSQL tab. `daemon.Build` builds one `pg.Service` (a `module.Module`), registers it and passes it to `ops.New`; every PG action goes through `ops.Service`, so the CLI and console share it.
* Connection resolves `postgres.dsn`, else the unix socket (`/var/run/postgresql`, then `/tmp`), else `127.0.0.1:5432`, with the libpq `PG*` environment merged by pgx; under sudo it impersonates `SUDO_USER`, since root has no matching role. A failed detection logs one warning and leaves the tab hidden. The service keeps a short-lived connection per inspection and a `connConfig` for the external `pg_dump`/`pg_dumpall`/`pg_restore`, whose password travels in the child environment, never argv.
* The inspector writes nothing and audits nothing. `pg.Service.BackupAll`/`BackupDatabase` dump each selected database in custom format and record a row in `state_dir/pg-backups.json` (never in SQLite). Uploads go through the `Uploader` seam backed by minio-go.
* Retention is the pure `keepSet` in `./internal/pg/retention.go`: GFS buckets by hour/day/ISO week/month. `prune` removes files, S3 objects and catalog rows beyond the policy; failed entries stay for visibility until the catalog cap drops them.
* `postgres` and `s3` are host keys but hot-reload: the console's `POST /api/pg/config` writes the selection to the host `appboss.local.yaml` (created from the base when missing) and calls `ops.Service.ApplyPGConfig`, so no daemon restart. A plain edit on disk applies on the next config save or restart.
* Runtime deps on the box: `pg_dump`, `pg_dumpall` and `pg_restore` on the service user's `PATH`, and a role that can read every selected database. Keep the S3 keys out of the committed file with `$VARS`.

## Console frontend (fez)

* Components live in `./internal/console/static/fez/`, one component per file, each loaded from `index.html` with its own `<script fez="/assets/fez/<name>.fez">` tag. Do not switch to a multi-component `<xmp fez>` file.
* The full-screen log viewer is a second entry page: `./internal/console/static/log.html` loads `ab-toast`, `ab-log-view` and `ab-log-shell`, and `ab-log-shell` exposes the same `Boss` global. `ab-log-view` is shared by that page and the in-console `ab-logs` tab.
* Read `~/dev/gems/fez/AGENTS.md` before editing any `.fez` file.
* Fez replaces a component tag with `<div class="fez fez-<name>">`; style that wrapper, not the custom tag.
* All CSS lives in `./internal/console/static/app.css`; components have no `<style>` blocks. Keep the light Tabler-style tokens defined on `:root` there.
* No external assets: the CSP allows only the console's own origin. `fez.min.js` is vendored; update it by copying https://dux.github.io/fez/dist/fez.min.js.
* Cross-component calls go through the globals `Boss` (shell: `api`, `reload`, `runAction`, `rescan`, `logout`), `Toast.show(message, error)` and `Drawer.open(title, text)`. Shared data lives in `globalState` (`apps`, `loaded`, `busy`, `status`, `restartRequired`).
* Console views are hash-routed in `viewFromHash` in `ab-shell.fez`; a new nav tab needs its own `location.hash === '#<view>'` case or its section never renders. `./internal/console/static_test.go` guards this.
* The Sys tab (`ab-sys.fez`) is read-only: it renders `sysinfo.Snapshot` from `GET /api/sys` and re-probes through `POST /api/sys/refresh`. Neither route audits; see `./internal/sysinfo` for the snapshot shape.
* The PostgreSQL tab (`ab-pg.fez`) is gated on `globalState.pgAvailable`, set from `capabilities.postgres` in `/api/bootstrap` and `/api/apps`, so it only appears when a server is reachable. It reads `GET /api/pg`, writes selection and policy through `POST /api/pg/config`, and runs `POST /api/pg/backup` and `POST /api/pg/restore`. The policy inputs are `fez:this` refs, never bound to rendered state, so typing never re-renders or resets them.
* The Realtime tab (`ab-pubsub.fez`) is gated on `globalState.pubsubAvailable` from `capabilities.pubsub`, which is true when any app sets a `pubsub.path`. It reads `GET /api/pubsub` and `GET /api/pubsub/secret`, and writes through `POST /api/pubsub/publish` and `POST /api/pubsub/rotate`; both publish and rotate audit. The app select and the publish inputs are `fez:this` refs, synced after render, never bound to rendered state.
* The config editor keeps the textarea and gutter under `fez:keep` and drives them with direct DOM writes; typing must never re-render the component.
* Check components with `bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'` and then look at the real page in a browser.

## Deploy hooks and one-off commands

* Hooks are named one-shot commands under `hooks:` in the app file, triggered by a signed POST to `/hooks/<app>/<hook>` on the management host. The endpoint lives in `./internal/console/console.go:handleHook` and runs before the session auth; its own secret is the credential (query token, bearer, `X-Gitlab-Token`, or a GitHub HMAC over the raw body).
* A hook with no config `secret` gets a 64-character generated one in `state_dir/hook-secrets.json`, owned by `./internal/hook`. The config secret wins; `RotateHook` rejects a config secret. `hookInfos` is the only path that carries secrets and is kept off the regular snapshot.
* Hooks reuse the cron runner (`jobState`/`jobRun` in `./internal/super/cron.go`); both live in `a.cron` and `a.hooks`. Output goes to a `hook-<name>` channel. `restart: true` restarts (or starts) the app after a clean exit.
* `appboss exec` runs off the app goroutine via `Manager.Exec`; it only reads the immutable spec through `requestExecInfo`, so a slow command cannot stall the supervisor.

## Console auth

* `./internal/console/auth.go` holds both sign-in paths: AuthCog (admins only) and `appboss login` (`/login?token=`, 3 minutes, single use, signs in as `cli@localhost`).
* `cli@localhost` is accepted only for sessions created by a token. The AuthCog callback must keep rejecting it.
* The console answers for `management.host` and for loopback names (`127.0.0.1`, `localhost`). A loopback request without a session gets a "run appboss login" page, never an AuthCog redirect. The login link targets `127.0.0.1:<first port of ports.range>`.
* The login link is minted through the control socket (`login` method in `./internal/ctl/server.go`), never over HTTP.
