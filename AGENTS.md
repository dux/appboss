# deploy-boss agent notes

Single Go binary (`dboss`) that supervises, proxies and logs the apps on one host.
Read `./README.md` for usage and `./doc/plan.md` plus `./doc/plan-v2.md` for the design before changing behavior.

## Working rules

* Build with `make build`; `~/bin/dboss` is a symlink to `./bin/dboss`, so a rebuild is what the shell runs.
* Validate with `make check` (vet and tests). Add a test next to the package you change.
* The demo host is `./demo/dboss.yaml`; run it with `make demo` or `dboss start` inside `./demo`. It listens on `127.0.0.1:8080`, so every hostname needs `:8080`.
* Console static assets are embedded with `go:embed`. A CSS or component change needs a rebuild and a daemon restart to show up.
* Do not add Docker, TLS, rate limiting or deploy logic. Cloudflare owns the edge, lux-deploy owns releases.
* Config is real YAML on disk. Never introduce a database copy of the config.
* `dboss.yaml` is the only config file name; `dboss.local.yaml` is the server-only override and is gitignored.
* A new config key needs a description (and an example when it has no default) in `keyDocs` in `./internal/config/keys.go`; the test fails otherwise. Path, type and default are read from the structs and `Default()`. Update `reference.yaml` for the long-form text.
* State files under `state_dir` (`running.json`, `maintenance.json`, `last_activity.json`) are written by the daemon only.

## Console frontend (fez)

* Components live in `./internal/console/static/fez/`, one component per file, each loaded from `index.html` with its own `<script fez="/assets/fez/<name>.fez">` tag. Do not switch to a multi-component `<xmp fez>` file.
* Read `~/dev/gems/fez/AGENTS.md` before editing any `.fez` file.
* Fez replaces a component tag with `<div class="fez fez-<name>">`; style that wrapper, not the custom tag.
* All CSS lives in `./internal/console/static/app.css`; components have no `<style>` blocks. Keep the light Tabler-style tokens defined on `:root` there.
* No external assets: the CSP allows only the console's own origin. `fez.min.js` is vendored; update it by copying https://dux.github.io/fez/dist/fez.min.js.
* Cross-component calls go through the globals `Boss` (shell: `api`, `reload`, `runAction`, `rescan`, `logout`), `Toast.show(message, error)` and `Drawer.open(title, text)`. Shared data lives in `globalState` (`apps`, `loaded`, `busy`, `status`, `restartRequired`).
* The config editor keeps the textarea and gutter under `fez:keep` and drives them with direct DOM writes; typing must never re-render the component.
* Check components with `bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'` and then look at the real page in a browser.

## Console auth

* `./internal/console/auth.go` holds both sign-in paths: AuthCog (admins only) and `dboss login` (`/login?token=`, 3 minutes, single use, signs in as `cli@localhost`).
* `cli@localhost` is accepted only for sessions created by a token. The AuthCog callback must keep rejecting it.
* The console answers for `management.host` and for loopback names (`127.0.0.1`, `localhost`). A loopback request without a session gets a "run dboss login" page, never an AuthCog redirect. The login link targets `127.0.0.1:<first port of ports.range>`.
* The login link is minted through the control socket (`login` method in `./internal/ctl/server.go`), never over HTTP.
