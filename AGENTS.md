# app-boss agent notes

Single Go binary (`appboss`) that supervises, proxies and logs the apps on one host.
Read `./README.md` for usage and `./doc/plan.md` plus `./doc/plan-v2.md` for the design before changing behavior.

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
* A long-running feature implements `module.Module` (`Name`, `Start`, `Close`) in `./internal/module`. `logstore.Store` and `./internal/ingest` are the current modules.
* The proxy pipeline is an ordered `[]proxy.Filter` built in `proxy.initFilters`. A new request filter is a function of that shape, inserted before the forward stage; the built-ins live in `./internal/proxy/filter.go`.
* Actions reachable from both the CLI and the console belong on `ops.Service` (`./internal/ops`), not in either transport.

## Log store

* One SQLite database per app at `log_dir/<app>/appboss.sqlite`: `requests`, `logs` and the `logs_fts` FTS5 index, plus `tail_offsets` for the file tailer. `./internal/logstore` owns the schema, batching, search and prune.
* `logs.source` is the channel: `stdout` (process output), `appboss` (appboss's own daemon log, in the reserved `_appboss` database), or `file` with `logs.process` holding the app log path. `log_retention` prunes requests and `file` rows; `stdout_retention` prunes `stdout`/`appboss`.
* The supervisor writes through `super.logWriter`, which owns the file, rotates by size and can `Seal` a segment; `Manager.SealLogs` exposes it. Do not rename a live process log from outside the supervisor.
* `./internal/ingest` seals stdout segments and tails every `*.log` under `<app dir>/log` on `daemon.log_ingest_interval`. App log files are app-owned: track offsets, never delete them. Parse changes belong in `ingest.ParseLine`.
* `log_retention` (default `336h`) covers requests and app log files; `stdout_retention` (default `3h`) covers stdout and the appboss daemon log; `log_retention: 0` disables the whole store for the app. The `appboss.sqlite` name is fixed; there is no config key for it.

## Console frontend (fez)

* Components live in `./internal/console/static/fez/`, one component per file, each loaded from `index.html` with its own `<script fez="/assets/fez/<name>.fez">` tag. Do not switch to a multi-component `<xmp fez>` file.
* The full-screen log viewer is a second entry page: `./internal/console/static/log.html` loads `ab-toast`, `ab-log-view` and `ab-log-shell`, and `ab-log-shell` exposes the same `Boss` global. `ab-log-view` is shared by that page and the in-console `ab-logs` tab.
* Read `~/dev/gems/fez/AGENTS.md` before editing any `.fez` file.
* Fez replaces a component tag with `<div class="fez fez-<name>">`; style that wrapper, not the custom tag.
* All CSS lives in `./internal/console/static/app.css`; components have no `<style>` blocks. Keep the light Tabler-style tokens defined on `:root` there.
* No external assets: the CSP allows only the console's own origin. `fez.min.js` is vendored; update it by copying https://dux.github.io/fez/dist/fez.min.js.
* Cross-component calls go through the globals `Boss` (shell: `api`, `reload`, `runAction`, `rescan`, `logout`), `Toast.show(message, error)` and `Drawer.open(title, text)`. Shared data lives in `globalState` (`apps`, `loaded`, `busy`, `status`, `restartRequired`).
* The config editor keeps the textarea and gutter under `fez:keep` and drives them with direct DOM writes; typing must never re-render the component.
* Check components with `bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'` and then look at the real page in a browser.

## Console auth

* `./internal/console/auth.go` holds both sign-in paths: AuthCog (admins only) and `appboss login` (`/login?token=`, 3 minutes, single use, signs in as `cli@localhost`).
* `cli@localhost` is accepted only for sessions created by a token. The AuthCog callback must keep rejecting it.
* The console answers for `management.host` and for loopback names (`127.0.0.1`, `localhost`). A loopback request without a session gets a "run appboss login" page, never an AuthCog redirect. The login link targets `127.0.0.1:<first port of ports.range>`.
* The login link is minted through the control socket (`login` method in `./internal/ctl/server.go`), never over HTTP.
