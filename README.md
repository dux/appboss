# dboss

Run all your apps on one server, from one Go binary.
Supervisor, router, HTTPS, log store and web console in a single file, configured by one `dboss.yaml` per app.

## Who this is for

You have a handful of apps and one decent server, and you want them online without operating a cluster.

dboss is for you if:

* You do not use Kubernetes, and do not want to learn it to run four Rails apps.
* You do not need a swarm of application servers, autoscaling or multi-region failover.
* One box with enough RAM is genuinely enough, and you would rather spend that RAM on your apps than on a control plane.
* You want to see what is running, read the logs and fix the config without stitching together five separate tools.

If you really do need a cluster, use a cluster.
dboss is deliberately a single-host tool, and it is very good at being one.

## What you get

All of it is in the one binary: no sidecars, no agents, no extra database, no YAML you did not write.

* **Routing built in.** Requests reach the right app by hostname, including wildcard and apex patterns, several web processes per app, canonical host redirects and static files served straight off disk.
* **Logging built in.** Every process log line and every request lands in a per-app SQLite database with full-text search, read from the console or from `dboss logs`, so there is no log pipeline to run.
* **Traffic built in.** Requests over time, error rate, latency quantiles and the top paths, status codes, countries and client IPs, per app.
* **Effortless deploys.** `dboss deploy sync` pushes the tracked files over ssh and restarts the app, `dboss deploy git` has the box pull and restart with nothing but a token, or push to GitHub or GitLab and a signed webhook does the same, with no runner and no pipeline credentials on the box.
* **Process supervision.** A procfile per app, workers alongside web processes, several copies of one process behind one host, restart policies with backoff, readiness and liveness checks, and rolling restarts that start the new release next to the old one, so a deploy never drops a request and a broken release never replaces a working one.
* **Apps that sleep.** An idle app stops on its own and the next request wakes it, so a dozen side projects share one box without holding RAM they are not using.
* **HTTPS without the chore.** Set one key and dboss gets Let's Encrypt certificates on demand for the hostnames it already serves and renews them, or put Cloudflare in front and let it terminate.
* **A real web console.** Live state, start and stop, log search, traffic, the config files edited on disk with revision history and restore, and an audit row for every action.
* **Scheduled jobs.** Cron per app, running even while the app itself is stopped, with output in the same log store.
* **Access control.** Basic auth, IP allowlists, or a full SSO sign-in in front of any app, without touching the app's code.
* **Batteries for the rest.** PostgreSQL inspection, a SQL prompt, scheduled backups, rotation and restore; realtime pubsub channels over WebSocket or SSE; Prometheus metrics with `/healthz` and `/readyz`; webhook alerts on crashes, restart loops, failed cron jobs, OOM kills, full disks, error rates and slow responses; per-process memory and CPU limits on a cgroup v2 host.

Install is one command and the service runs as an ordinary user, not root.
`dboss start` on your laptop gives you the same thing locally, with no sudo and no setup.

## How it works

Each app is a folder with a `dboss.yaml` naming its processes and hostnames.
dboss reads them, hands every process a fixed `PORT`, proxies HTTP to the right one by hostname, stops idle apps and wakes them on the next request, and ingests every process log and request row into that app's log store.
Daemon features are modules with a common lifecycle, so a new one (an ingestion sink, a security filter) plugs in at one place.

There are no containers.
Deploys are two commands, `dboss deploy sync` and `dboss deploy git` (see [Deploying](#deploying)); lux-deploy, when you use it for releases and rollback, calls `dboss restart` at the end of a deploy.
The configuration reference ships in the binary: `dboss config --reference`, also embedded from `./internal/config/reference.yaml`.

## Requirements

* Go 1.25 or newer to build.
* Linux for production, macOS for development.
* Nothing else at runtime: the binary embeds the console assets and the configuration reference.

## Install

One script installs the binary and, with a flag, sets up the host it runs.
It asks GitHub for the latest release, downloads the `linux` or `darwin` build for the machine's architecture (`amd64` or `arm64`), verifies it against the release checksums and installs it as `/usr/local/bin/dboss`.

```sh
curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh          # binary only
curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh -s -- --help
```

Set `DBOSS_INSTALL_DIR` to change the target, or `DBOSS_VERSION` (for example `v0.1.0`) to pin a release.

### Development (macOS or Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sh -s -- --dev
cd ./dboss && dboss start
```

`--dev` creates `./dboss/apps` and writes a starter `./dboss/dboss.yaml` from `dboss init service`; `--dir` puts it somewhere else.
Nothing is installed as a service and no `sudo` is needed: on a terminal a `proxy.listen` port this session may not bind moves to the first free port of `ports`, and the daemon logs the address it took.
Stop it with Ctrl-C, which stops every app with it.

### Production (Linux)

```sh
curl -fsSL https://raw.githubusercontent.com/dux/dboss/main/install.sh | sudo sh -s -- --server --user deploy
```

`--server` needs root once, to write `/etc/systemd/system/dboss.service`. Everything after that runs unprivileged. It:

* creates `/srv/dboss/apps` and a starter `/srv/dboss/dboss.yaml` when they are not there yet (`--dir` moves the host),
* gives the whole directory to `--user`, so the runtime `dir`, the certificate cache and the generated pubsub secrets belong to the service user from the first start,
* runs `dboss check` as the gate, then `dboss systemd --install` to write, reload and enable the unit.

The user must already exist - reuse the account lux-deploy rsyncs with, so releases, app-written files and logs all have one owner and the control socket needs no group setup.
The unit runs the daemon as that user with `CAP_NET_BIND_SERVICE`, so it binds `:80` and `:443` without being root.

Then set `management.host`, `management.admins` and `tokens.dboss` in `/srv/dboss/dboss.local.yaml` (server-only, gitignored, never touched by a deploy), restart, and sign in:

```sh
sudo systemctl restart dboss
dboss login
```

Check it came up unprivileged:

```sh
systemctl status dboss
ps -o user= -p $(systemctl show -p MainPID --value dboss)   # the deploy user, not root
dboss doctor
```

### Updating

```sh
dboss update --check        # is there a newer release?
sudo dboss update           # download it, verify it, replace the binary
sudo systemctl restart dboss
```

`dboss update` does what the install script does, without the host setup: it asks GitHub for the latest release, downloads the asset for this platform, verifies its sha256 against the release `checksums.txt` and renames it over the running executable.
A failed download or a checksum mismatch leaves the old binary exactly where it was.
It follows a symlink to the real file, so `~/bin/dboss` updates what it points at, and it stops with a `sudo dboss update` hint rather than elevating itself when the binary directory is not writable.
`--version <tag>` installs a named release instead of the latest.

The running daemon keeps the old binary in memory until it is restarted, which is why the restart is a separate step.

### Versions

The version is the number of commits in `main` when the binary was built, printed as `v<count>`:

```sh
$ dboss version
v81
```

There is nothing else to it - no `major.minor.patch`, and the release tag carries the same number, so `dboss update` compares two integers.
`make build` and `.github/workflows/release.yml` both inject it; a binary built straight from source with `go build` reports `dev`, and `dboss update` refuses to replace one without `--force`.

Releases are built by `.github/workflows/release.yml` on every `v*` tag push; building from source is still the option below and needs Go 1.25+.

## Build and run the demo

```sh
make build            # ./bin/dboss
make demo             # builds, then runs the host session on ./demo/dboss.yaml
```

The demo listens on `:80` and hosts three apps, and it needs no `sudo`.
Binding port 80 normally takes root or `CAP_NET_BIND_SERVICE`, but on a terminal a `proxy.listen` port the process may not bind moves to the first free port of `ports` instead of failing.
The startup banner names the address every app ended up on, so when the demo falls back the URLs below need that port, for example `http://bun.lvh.me:3101`.

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
A folder without either file is also searched in its `config/` subfolder, so an app (a Rails app, say) can keep `config/dboss.yaml`; relative paths still resolve against the app folder, and files in both places are an error.
Every command looks for the config as `-c path`, then `$DBOSS_CONFIG`, then the current folder.

Every host key has a sane default - `apps: ./apps`, `dir: ./.dboss`, `proxy.listen: ":80"`, `ports: [3100, 3990]`, the AuthCog realm, session lifetime and the daily maintenance time - so a host file only names what deviates. The config has no tuning knobs: timeouts, rotation sizes and check cadences are built in, and a key dboss dropped fails `dboss check` with its replacement. With no config file at all, `dboss start` runs the default host: `:80`, `./apps`, console off.

Host file (`./demo/dboss.yaml`):

```yaml
management:
  host: dboss.lvh.me
  admins:
    - you@example.com

ports: [3100, 3199]

tokens:
  github: $GITHUB_TOKEN   # outbound: dboss pulls private repos with it
  dboss: $DBOSS_TOKEN     # inbound: hook pings and /metrics present it
```

App file (`./demo/apps/bun/dboss.yaml`):

```yaml
procfile:
  web:
    command: ./start.sh
    hosts: [bun.lvh.me]
    health: /up
```

The proxy listens on `:80` by default and owns that port for every app; the demo uses the same address, so a hand-run session needs root or `CAP_NET_BIND_SERVICE`.
When it does not have either and stdout is a terminal, the proxy falls back to the first free port of `ports` instead of exiting, so developing against an app needs no sudo; under systemd stdout is a pipe and the bind failure is still fatal.
A dev session (`dboss start` in an app folder) never takes `:80` unless asked: it ignores `proxy.listen` and serves on the next free port, so the app opens as `http://myapp.lvh.me:3110`.
`dboss start --https` listens on `:80` and `:443` instead and fails when either is taken.
Every process that declares `hosts` is a web process, and an app may have several, each serving its own hostnames; a process with only a command is a background worker. Running dboss inside an app folder with no hosts binds the first process to `.lvh.me`.
Every key that takes a list also accepts a single value, so `allow_ips: 10.0.0.0/8` equals `allow_ips: [10.0.0.0/8]`.
A leading `*.` in a host matches subdomains only; a leading `.` matches the bare domain and every subdomain, so `hosts: .myapp.com` covers `myapp.com` and `*.myapp.com`.
The web process can also set `canonical_host` (one of its hosts); every other host answers 301 to it, so `www` never serves content.
`proxy.listen` and `management.host` are such lists: several listen addresses each get a listener with the same routing, and several console hostnames are all accepted.
A `$NAME` in a value is replaced with that variable from the daemon's environment at load time, so `url: $ALERT_WEBHOOK_URL` keeps a secret out of the file; only all-uppercase names expand, an unset name stays as written, and `procfile` and cron commands are never expanded because they are runtime shell lines.
Any key can be written twice, once with a `_dev` suffix: a dev session - one app run from its own folder, `dboss s` where the config file has `procfile` - takes the `_dev` value, and every other session drops it, so one committed app file serves both.

```yaml
procfile:
  web: bundle exec puma -e production
  web_dev: bundle exec puma -e development
env:
  API_URL: https://api.example.com
  API_URL_dev: http://lvh.me:4000
```

The suffix works at every depth and inside free-form maps such as `procfile`, `env` and `headers`; the value replaces the base key outright, so a block override names the leaf key it changes (`alerts: {error_rate_dev: 0}`) rather than restating the block. A `<key>_dev` is checked against the schema in both modes, so a typo is caught by `dboss check` on the host too.
Every app-level key can be set once under `defaults:` in the host file and repeated at the top level of an app file; the app value wins key by key.
`dboss config --keys [filter]` lists every key grouped by block, with a one-line description, its default and, when useful, an example; the same list is behind the Help button in the console's Configuration view.
`dboss config --reference` prints the long annotated reference, and `dboss config [app] -d` prints a resolved config with every default filled in.
`dboss start --https` adds HTTPS on `:443` to a dev session, so an app that needs a secure origin (secure cookies, service workers, OAuth callbacks) works locally, with certificates from a local certificate authority dboss keeps in your user config directory and shares across projects. Nothing is issued by Let's Encrypt and plain http keeps working. The first `--https` start on a terminal asks whether to trust that root (`[Y/n]`, it may ask for your password) and starts either way; `dboss trust` does the same at any time, adding it to the macOS login keychain or the Debian/Fedora store through `sudo`. Until it is trusted the banner says so and the browser warns.
`dboss init` prints a fully commented starter config, service or app, with every key shown with its default or an example; save it with `dboss init > dboss.yaml`.

```
$ dboss config --keys tokens
Tokens  (root dboss.yaml)
  tokens.github  outbound: personal access token a pull hook and a github_pr preview use for a private repo; consumed from the process environment only  e.g. $GITHUB_TOKEN
  tokens.dboss   inbound: every /hooks ping and /metrics must present it; unset refuses hooks and hides /metrics                                    e.g. $DBOSS_TOKEN
```

## Commands

```
dboss <command> [options]
dboss help <command>

Host session
  start         run the host session in the foreground; Ctrl-C stops every app
  systemd       print the systemd unit for this config, or install and enable it
  kill          stop every app and terminate every listener left in ports
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
  hooks         list an app's deploy hooks with their ping URL, or run one
  deploy        push an app to a box: sync over ssh, or git pull through the deploy hook
  exec          run a one-off command in the app's environment
  audit         list operator actions: start, stop, restart, destroy, hook runs and config writes

Config
  init          print a fully commented starter config for a service or an app
  config        print a config file, the resolved config, the key reference, or saved revisions
  check         validate the config and every app without starting anything
  pages         list the pages dboss serves and which file renders each, or write them out to edit
  doctor        preflight a box: tools, writable dirs, valid config and a clear port range
  rescan        re-read the apps directory, every dboss.yaml and the host defaults
  ports         show the live port table, one fixed port per process slot
  password      print a bcrypt hash for basic_auth
  sshkey        list the local SSH public keys, or create a new key

Binary
  version       print the dboss version
  update        download and install the latest dboss release
```

`dboss start` always runs in the foreground; systemd is the daemonizer and `dboss systemd --install` writes and enables the unit.
Every other command talks to the running host over its control socket and accepts `--json`.
Inside an app folder the app argument defaults to that app.

On a terminal, `dboss start` opens with the dboss version and one row per process, then waits for ENTER before it starts any app:

```
dboss v113 - host /Users/me/dboss:
bun/web       http://bun.lvh.me
button/web    http://button.lvh.me
sinatra/web   http://sinatra.lvh.me
sinatra/job   worker
dboss/console http://127.0.0.1:3100/login?token=...  signed in for an hour
Press ENTER to start the apps (dboss start -y skips this)
```

Every listener already answers at that point, so the console and each URL can be opened first; a page requested before ENTER gets the starting page, which reloads itself into the app once it is up.
`dboss start -y` starts the apps right away, and so does a start without a terminal on stdin.

Each row is keyed by the same colored `app/proc |` prefix that process logs under, so the address and the output that follows it line up:

```
sinatra/web | == Sinatra (v4.1.1) has taken the stage on 3101
sinatra/job | tick
```

Inside an app folder there is only one app, so the key drops to the process name:

```
$ cd ~/apps/sinatra && dboss s
dboss v113 - dev session for sinatra:
web     | http://sinatra.lvh.me:3110
job     | worker
console | http://127.0.0.1:3101  open on this machine
Press ENTER to start the apps (dboss start -y skips this)
web     | == Sinatra (v4.1.1) has taken the stage on 3107
job     | tick
```

A web process shows the address to open (its `canonical_host`, else its first hostname), a worker says `worker`, and an app in maintenance says so.

Dev sessions in several app folders run side by side.
Each one claims the next free ports for its console, processes and proxy from a per-user registry in the user config directory, so no two sessions share a port and none clears another's listeners.
A folder gets the same ports back on its next start while they are free, so its URLs stay put; its own record is `.dboss/state/ports.json`.
A dev start only clears what an earlier run of the same folder left on those ports, and `dboss kill` inside the folder clears only them.
The first row is the console: a dev session prints its plain loopback address, since it needs no sign-in, and a host session prints a sign-in link that lives for an hour and can be clicked more than once, unlike the single-use link `dboss login` prints.
Under systemd none of this appears and no link is minted: the banner, like the output echo and the privileged-port fallback, only happens when stdout is a terminal.

A hand-run session also warns once when the runtime folder would be committed:

```
dboss: .dboss holds this host's state, logs and secrets and is not gitignored
       add it: echo .dboss/ >> /Users/me/apps/myapp/.gitignore
```

`dir` defaults to `.dboss` in the config directory and holds `state/`, `log/` and the control socket: the request and log databases, the generated pubsub secrets and the certificate cache.
The check only runs when that directory has a `.gitignore` of its own, and it asks `git check-ignore`, so a rule in a parent directory, in `.git/info/exclude` or in your global excludes counts.
A host whose `dir` lives outside the checkout never sees it.

### Startup and the running list

On start the host clears every listener in `ports`, then starts the apps listed in `dir/state/running.json` that have `autostart: true` (the default).
That file is written on every `run` and `stop`, so an app you stopped stays stopped across restarts.
When the file does not exist yet, which is the case on a first start, every discovered app with `autostart: true` is started.
An app with `autostart: false` stays down across host restarts until `dboss run`, the console, or the first proxied request starts it.
An app with `autostart: button` also stays down, but a request answers a page with a start button and only its POST starts the app, so a crawler or a favicon request never does.
A stopped app is also started by the first proxied request, which gets the `starting` page; it reloads every 5 seconds until the app answers.

## Logs

Every app has one SQLite database at `dir/log/<app>/dboss.sqlite` with three tables:
`requests` (one row per proxied request, written by the proxy), `logs` (one row per log line,
written by the ingestion module) and `tail_offsets` (how far the file tailer has read).
`logs` carries `ts`, `source`, `process`, `stream`, `level`, `message`, `request_id` and `raw`,
and an FTS5 index over `message` and `raw` backs the text search.
`requests` carries `request_id` (the `CF-Ray` when Cloudflare sent one, so a request from the Cloudflare dashboard can be found by pasting its Ray ID into the search) and `country` (the two-character `CF-IPCountry`, empty without it).

Each row belongs to a channel and the console's **Logs** viewer selects one:

* `REQUEST` - the proxy's request rows.
* `STDOUT` - the stdout/stderr of each app process, sealed and parsed by the ingestion module.
* `dboss` - dboss's own daemon log, mirrored into the reserved `dir/log/_dboss` database and
  offered as **Host (dboss)** in the app picker.
* one channel per `*.log` file the app writes under `<app dir>/log`, tailed by byte offset and
  never rotated or deleted.

`REQUEST` rows and app log files are kept for `log_retention` (default `336h`, two weeks);
`STDOUT` and the dboss daemon log for `stdout_retention` (default `3h`). Both are deleted by the
daily prune; `log_retention: 0` disables the store for the app.
The prune runs daily at `maintenance_at` (default `04:10`) and is followed by SQLite `VACUUM` on every app database and the host database to reclaim the freed space, including databases left behind by apps removed from the config.
Process log files rotate at 10m and keep five rotated files.
The supervisor owns the process log file: every 5 seconds it
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

The **Logs** route (the **Logs** button on an app card opens `#/logs?app=<name>` in a new tab)
filters by channel, time range, level or HTTP method/status and free text, highlights matches,
expands a row to its raw fields and exports the current query as text.
The current filters live in the hash query, so a view can be bookmarked, shared or reached with Back.

## Temporary files

An app's `./tmp` is where caches, uploads, sockets and pids pile up, and nothing ever comes back
for them.
Once a day, and once when the daemon starts, dboss deletes the files under `<app dir>/tmp` that
were last modified longer than `tmp_clean` ago (default `7d`), then the sub-directories the
deletion left empty and the ones that were already empty and as old.
The `tmp` directory itself always stays, an app without one is skipped, and a `tmp` that is a
symlink to a shared directory is followed.

```yaml
tmp_clean: 30d              # or a plain duration: 72h
tmp_clean: false            # never touch ./tmp; 0 means the same
```

The key is valid under host `defaults:` and at an app's top level, like the other runtime keys.
Age is the file's mtime, so anything a long-running process wrote once and still uses is cleaned
like the rest; set `tmp_clean: false` for an app that keeps something there for longer.

## Disk usage

Every app is measured once when the daemon starts and once a day after that: its own directory
plus `dir/log/<app>`, the process logs and the SQLite log store dboss writes for it.
The console card shows the total next to the memory stat, with the split and the measurement time
in its tooltip; clicking the value measures that app again on the spot, which is what to do after
a cleanup rather than waiting for the next pass.
`/metrics` carries the same numbers as `dboss_app_disk_bytes{app,part="app"|"logs"}` plus
`dboss_app_disk_measured_timestamp_seconds`, and an app that has not been measured yet is left out
of both instead of being published as zero.

A walk of a release tree is far too slow for a page load, so the value is always the cached one.
It is apparent size, what `du --apparent-size` prints: two hard links to one file count twice.
An app entry that is a symlink to the current release measures that release, not its siblings, and
a `tmp` or `uploads` symlink out of the app counts as the link, so a shared directory is never
billed to two apps.
In a single-app session `dir/log` sits inside the app folder; the log store is still counted once,
and the rest of `.dboss` (state, config history, certificates) lands in the app half.

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

## Deploying

Two commands, both run inside the app folder on your machine:

```
dboss deploy sync deploy@box.example.com:/srv/dboss/apps/myapp
dboss deploy git https://dboss.example.com        # token from --token or $DBOSS_TOKEN
```

`sync` ships exactly the files git tracks (`git ls-files`), with their working-tree content, uncommitted edits included.
Untracked and ignored files never leave your machine: `.env`, `.env.local`, `dboss.local.yaml` and scratch files stay local, and a file the box needs is placed there on purpose.
Untracked files that are not ignored are counted in one `skipped N untracked files` line, so a missing `git add` is visible.
rsync (`--files-from`) copies the changed files, then `dboss deploy apply` runs on the box over ssh: it removes the files an earlier sync shipped and this one no longer does, records the list in `.dboss-sync` in the app folder, and restarts the app.
Files that only exist on the box (logs, uploads, `.env.local`, `dboss.local.yaml`) are never touched, and the first sync removes nothing.
rsync's own `--delete` is not used on purpose: openrsync, the macOS default, removes gitignored files on the receiver with it.
`-n` shows what would be copied and removed without restarting; the app name is the remote folder name unless `--app` says otherwise.
It needs `git`, `rsync` and `ssh` locally, and `rsync` plus `dboss` on the ssh user's `PATH` on the box; ssh in as the service user.

`git` needs no ssh.
It posts to the app's `deploy` hook (`hooks: {deploy: true}`, see below) with `tokens.dboss` as a bearer token, then polls `GET /hooks/<app>/deploy` until the pull and the restart are done, and prints the hook's output.
A failed pull prints git's answer and exits with its code; an app without the hook gets the line to add.

## Deploy hooks

An app can declare one-shot commands a signed HTTP ping triggers, so a Git host webhook can start a deploy without any shell access:

```yaml
hooks:
  deploy:
    command: git -C .. pull --ff-only
    timeout: 10m
    restart: true      # restart the app when the command exits 0
    overlap: false      # skip a ping while the previous run is still going
```

A hook can also be written as the bare boolean `deploy: true`, shorthand for `git pull --ff-only` in the app folder plus `restart: true`; `git` must be on the service user's `PATH`, and a non-fast-forward update or a dirty tree fails the hook without restarting. `dboss hooks [app]` shows the resolved command.

For a private repo, set `tokens.github` (a PAT) in the host file; write `$GITHUB_TOKEN` to keep it out of the file. dboss hands it to the pull through the environment only, via a credential helper (git 2.31+), so it never lands in argv, the repo's config or the app's processes; with no token the pull stays anonymous.

The ping URL is `https://<management.host>/hooks/<app>/<hook>`. Every ping presents `tokens.dboss` from the host file, a value you choose (for example `openssl rand -hex 32`) and paste into the sender: `?token=<token>` in the URL, `Authorization: Bearer`, `X-Gitlab-Token` (GitLab's Secret token field), or a GitHub `X-Hub-Signature-256` HMAC over the raw body (GitHub's Secret field). Without the token every ping answers `401`. `X-GitHub-Event: ping` (sent when the webhook is created) is acknowledged without running anything.

`dboss hooks [app]` lists hooks with their last result and the ready-made ping URL; `dboss hooks run [app] <hook>` starts one now. To change the token, edit it and run `dboss rescan`; the old URLs stop working. Hooks run in the app folder with the app environment, log to a `hook-<name>` channel, and leave the app alone unless `restart: true`.

`GET /hooks/<app>/<hook>` with the same token answers the hook's last result, whether it is `running` or `restarting` the app, and the tail of its last output; `dboss deploy git` polls it.

`dboss exec [app] <command> [args...]` runs a one-off command in the same environment and prints its combined output. Options come before the command, so the command's own flags pass through; `--timeout` (default 1m) kills it, and its exit code becomes dboss's exit code.

## Lifecycle steps

An app can name the commands dboss runs at three points of its life, so it sets up and cleans up what it owns, such as its database:

```yaml
lifecycle:
  create: bin/setup-db              # once, before the first start
  start:                            # before every start; processes wait for it
    command: bundle exec rake db:migrate
    timeout: 10m                    # default 3m
  destroy: bin/drop-db              # after dboss destroy stops the app
```

Each step runs in the app folder with the cron and hook environment (no `PORT`) and logs to a `lifecycle-<step>` channel.

* `create` runs inside the first start, before `start`. The app is recorded in `dir/state/created.json` only when it exits 0, so a failure retries on the next start, and destroy clears the record. An app that already existed runs it once on its next start, so write it to be idempotent.
* `start` runs on every start and restart, including a wake by the proxy. The app stays `starting` meanwhile and its processes spawn only after a clean exit. A process restarted after a crash does not rerun it.
* A failed or timed-out `create` or `start` leaves the app `crashed` with the step's output in the error log and sends the `crash` event. Stopping the app kills a step still running.
* `destroy` runs after `dboss destroy` has stopped and detached the app, before its folder is removed. It is cleanup: a failure is logged and sends `hook-failed`, and the destroy still completes.

## GitHub PR previews

A host can declare one built-in `github_pr` hook that turns a branch into a short-lived app, so a Git host webhook creates, updates and tears down PR previews with no runner and no SSH deploy script.
The hook is host-level and answered at `https://<management.host>/hooks/github_pr`, signed with `tokens.dboss` like every hook; one path segment is a host hook, two are an app hook.

```yaml
hooks:
  github_pr:
    repo: https://github.com/owner/repo.git       # fallback when the ping omits repo
    template:
      name: $QS_BRANCH
      hosts: [pr-$QS_BRANCH.example.com]
      autostart: false
      deletable: true
      lifecycle:
        create: bin/setup-db      # once, on the first deploy
        destroy: bin/drop-db      # when the PR closes
      procfile:
        web: {command: ./start.sh, health: /up}
```

The ping carries the branch and a few query params: `action`, `branch`, `repo`, `num`.
A GitHub Action step can send them straight from the event, so nothing has to parse the webhook body:

```sh
curl -fsS -X POST "https://dboss.example/hooks/github_pr?token=${{ secrets.DBOSS_TOKEN }}&action=${{ github.event.action }}&branch=${{ github.head_ref }}&repo=${{ github.event.pull_request.head.repo.clone_url }}&num=${{ github.event.number }}"
```

Every query param becomes `QS_<NAME>` for the hook.
`action=closed` destroys the app, which runs its `destroy` step and removes the checkout, so the template needs `deletable: true`; any other action checks out the branch tip, writes the app config from `template`, and restarts the app, which runs its `create` and `start` steps.
A private `repo` over HTTPS is pulled with `tokens.github`; a fork PR works because the caller passes the head repo's clone URL.
Events for one branch are serialized, so two pushes cannot race the same checkout, while different branches deploy in parallel.

`template` is an app file with `$VAR` and `${VAR}` interpolation.
A value in a `hosts` or `canonical_host` field is sanitized as a DNS label (`/` and `_` become `-`); every other field is an identifier (`/` becomes `_`).
A top-level `hosts` is the default for every procfile entry that declares none.
The preview-only `name` key names the app folder, sanitized the same way, and `main` and `development` are refused.

Every step is audited under actor `hook:github_pr` (`config-write`, `deploy`, `stop`, `destroy`).
The preview runs like any other app, so the console, request log, metrics and `dboss ls` all see it.

## PubSub channels

A web process can serve a pub/sub hub on its hosts. Set `pubsub` and dboss answers the path instead of forwarding, so subscribers connect even while the app is stopped and realtime traffic never wakes it. `pubsub: true` uses `/socketio`, `pubsub: /path` sets a custom prefix, and a mapping sets the full options:

```yaml
procfile:
  web:
    command: ./start.sh
    hosts: [myapp.com]
    pubsub: true            # or /socketio, or a mapping
    # pubsub:
    #   path: /socketio
    #   secret: $PUBSUB_SECRET   # bearer for HTTP publish; empty generates one per web process under dir/state
    #   replay: 10               # messages kept per channel and replayed to a late subscriber
    #   max_clients: 500         # subscriber cap per hub; 0 means unlimited
    #   max_message_size: 64k    # largest publish body; 0 means unlimited
    #   client_events: true      # a WebSocket client may publish to its own channel
    #   test: false              # serve the browser self-test at <path>/_test
```

`pubsub` lives on a web process, because the hub is served on that process's hosts. An app with several web processes may run one hub per process, each with its own path, secret and channels.

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

The secret is accepted as `?token=`, `Authorization: Bearer` or `X-Pubsub-Token`, and satisfies a publish even when the app sets `basic_auth`. With no `secret` in the config, dboss generates a 64-character one per web process under `dir/state/pubsub-secrets.json` on first use; `dboss pubsub` prints it, and `dboss pubsub rotate [app]` replaces it.

**Self-test.** With `test: true`, `GET <path>/_test` serves a page that opens a WebSocket and an SSE connection and reports PASS or FAIL in the browser.

`dboss pubsub [app]` lists channels and subscriber counts, `dboss pubsub secret [app] [--process name]` prints the credential and example URLs, `dboss pubsub publish [app] <channel> [--event name] [--data json|-] [--process name]` sends a message through the control socket, and `dboss pubsub help` prints the integration guide. Name `--process` only when the app runs several hubs. The console's **PubSub** tab (when any web process sets `pubsub`) shows the same and can publish a test message. Channels are one path segment; `client.js`, `_test` and `_selftest` are reserved. Metrics are `dboss_pubsub_clients`, `dboss_pubsub_channels` and `dboss_pubsub_messages_total`, each labeled by app.

## Health and metrics

The management host also serves three endpoints:

* `GET /healthz` - `200 ok` while the daemon is up.
* `GET /readyz` - `200` only while every `autostart` app serves (running, or asleep and woken by the next request), else `503` with the apps that are not ready.
* `GET /metrics` - Prometheus text: build info, per-app up/state/uptime/memory/CPU, per-app disk usage by part with the time it was measured, per-process restarts and memory, request rates per window, request duration quantiles (p50/p95/p99 over the last hour), and the last exit of each cron job and hook.

`healthz` and `readyz` are open so an uptime checker or load balancer can reach them. `metrics` requires `tokens.dboss` as `Authorization: Bearer <token>` and answers `404` when no token is set. All three answer on the management host only.

Each app also answers on its own hosts at `health_endpoint` (default `/.well-known/dboss/health`): `200 {"app","state"}` while a visitor would be served, `503` otherwise. An app stopped by `idle_stop` (or `dboss stop`) still answers `200` with `"state":"stopped"`, because the next request wakes it, so a Cloudflare Health Check or Load Balancer never flags a sleeping app. Draining, maintenance, starting, crashed and a stopped `autostart: button` app answer `503`. It runs before basic auth and never wakes a stopped app, so a Cloudflare health check or uptime monitor can probe the app domain directly. Set `health_endpoint: ""` to disable it.

The supervisor also watches each web process for its whole lifetime: the `health` path declared on the web procfile entry (e.g. `/up`, or omitted for a TCP connect) gates startup readiness within `health_timeout`, polled every 500ms, because that decides how long a visitor who woke the app waits on the starting page. Once the process answers, the same check keeps running at the slower `liveness_interval` (default `10s`, and `5m` in a hand-run session, where a developer watching one app does not need it polled every ten seconds and every poll lands in their own request log), so a healthy app is not asked twice a second for its whole life; after `unhealthy_threshold` consecutive failures (default `3`) the process is killed and the normal restart policy, backoff (1s doubling up to 60s) and `max_restarts` apply. Set `unhealthy_threshold: 0` for startup-only readiness. Background workers are not polled.

`dboss doctor` preflights a box before a first start or a deploy: it checks that `lsof` is on `PATH`, that `dir` and its `state` and `log` folders are writable, that the config and every app load, and whether anything still listens in `ports` (a warning, since a start clears it).
It also names any listener in that range bound to a public address (`*:3101`, `0.0.0.0`, a LAN IP) rather than `127.0.0.1`.
An app binds its own port - dboss injects `PORT` and never an interface - and the proxy always dials `127.0.0.1`, so a public bind is a second door into the app that answers without `basic_auth`, `allow_ips`, the `auth` sign-in gate or the `X-Dboss-User` strip.
Bind loopback in the procfile (`puma -b tcp://127.0.0.1:$PORT`, `gunicorn -b 127.0.0.1:$PORT`, `next start -H 127.0.0.1`) or firewall the range.

## Notifications

A host can post runtime events to one operator webhook:

```yaml
notify:
  url: $ALERT_WEBHOOK_URL   # Slack, Discord and ntfy URLs get their own payload, anything else JSON
  events: [crash, restart-loop, health-timeout, wake-failed, hook-failed, cron-failed, deploy, config-changed, backup-failed, error-rate, slow, oom, disk-low]
  headers: {}
```

`crash` is an app entering the crashed state, `restart-loop` a process failing again after a restart, `health-timeout` the readiness check giving up or the web process failing its liveness checks, `wake-failed` a request that could not start a stopped app, `hook-failed` a deploy hook that exited non-zero, `cron-failed` a cron job that exited non-zero, timed out or could not start, `deploy` a `restart: true` hook that succeeded and rolled the app, `config-changed` a config write that changed a host key and needs a restart, `backup-failed` a PostgreSQL dump that failed, `error-rate` an app answering with too many 5xx, `slow` an app whose p95 latency crossed its limit (both from the app's `alerts:` block, checked once a minute over the last 5 minutes once there are 20 requests, default `error_rate: 10` percent and `slow_p95` off), `oom` a process the kernel OOM killer stopped at its `memory_max` (cgroup backend only), and `disk-low` a filesystem holding the config or the runtime folder filled past `disk_alert` percent (default 90, `0` off, checked once a minute). Sends are queued and best-effort, so a slow or dead endpoint never blocks the supervisor, and one event for one app is posted at most once every 5 minutes. The delivered/failed/dropped counts are exported as `dboss_notifications_total`. `url: ""` (the default) disables notifications.

## PostgreSQL inspection and backups

When a PostgreSQL server is reachable, the console gains a **PostgreSQL** tab and the daemon can back up selected databases on a daily run.

Configuration is one host-level block. The only backup setting is the per-database rotation window:

```yaml
postgres:
  dsn: $DATABASE_URL            # empty auto-detects the local socket, then 127.0.0.1:5432
  backups:
    myapp_production: week      # keep 7 days; month keeps 30
    reports: month
```

`postgres: false` turns the whole feature off.

The connection resolves in order: `postgres.dsn` when set, then a unix socket (`/var/run/postgresql`, then `/tmp` for Postgres.app), then `127.0.0.1:5432`, with the libpq `PG*` environment merged in. A daemon started with `sudo` runs as `root`, whose matching Postgres role does not exist, so detection impersonates the invoking `SUDO_USER`; set `postgres.dsn` explicitly when the service user has no matching role. The tab shows the server version, uptime, connection count, cache hit ratio, WAL LSN, replication state, live activity including the longest query and lock waits, and every database with its size, owner and last backup.

Backups are per-database logical dumps (`pg_dump --format=plain`) zipped as `pg_backup/<database>/BACKUP_<timestamp>.zip` next to the apps. One run happens each day at 04:00 local time; scheduled dumps older than the database's rotation window (7 days for `week`, 30 for `month`) are pruned from disk and the catalog, while a manual **Back up now** is kept. The catalog lives at `dir/state/pg-backups.json`.

Every recorded dump has **Download**, which serves the stored zip as it is, and the per-database panel has **Upload backup**, which stores an archive you picked and records it as a manual entry.
Together they move a database between hosts: download on one box, upload on the other, restore there.

Each database page has two tabs: **Backup** is everything above, and **SQL** is a runner.
It executes whatever you type against that database as the role dboss connects with - `Cmd/Ctrl+Enter` runs, several statements run in order and the last result set is shown, `NULL` is rendered as such, and long results stop at 500 rows with the full count reported.
A run is bounded by a 30 second `statement_timeout`, so closing the tab cannot leave a query burning CPU, and it can write, so every run lands in the audit log as `pg-query` with the statement.

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

* Keep container ports outside `ports`. On start dboss clears every listener in the range and before each spawn frees the app's fixed port, so a container listening there would be killed.
* A hostname is routed by one proxy only. dboss routes just the hosts of the apps in its own `apps` directory, so a container host must be served by Cloudflare or another reverse proxy.

The one bridge without code is a procfile wrapper (`docker run -p 127.0.0.1:$PORT:$PORT ...`), which makes a container answer as a dboss app but leaves its lifecycle on the docker CLI, with the usual caveats around stopping it.

## Access control

`basic_auth` puts HTTP basic auth in front of the whole app, static files included.
It maps a user to a plain password or a bcrypt hash printed by `dboss password`; set it under `defaults:` in the host file to protect every app on a staging box with one block.
`allow_ips` limits the app to a list of CIDRs (address ranges such as `10.0.0.0/8`), matched against the client address.
Behind Cloudflare set `proxy.cloudflare: true` in the host file: only Cloudflare's published ranges (built in) and the box itself may connect, and the client address comes from `CF-Connecting-IP`, which then cannot be spoofed.

```yaml
basic_auth:
  alice: "$2a$10$..."   # dboss password
  bob: secret           # plain password
allow_ips:
  - 10.0.0.0/8
```

`auth` puts an AuthCog sign-in in front of the app, the way Cloudflare Access does, for people instead of shared passwords.

```yaml
auth: [ana@example.com, "*@example.com"]   # empty leaves the app open
session_ttl: 24h                            # also under defaults:, where it covers the console
```

A visitor without a session is sent to AuthCog (`authcog_realm` in the host file), returns to `/.well-known/dboss/auth` and gets a signed, host-only cookie; `/.well-known/dboss/logout` signs out.
Only the listed emails and `*@domain` patterns get in (`"*"` admits any AuthCog account), and the list is checked on every request, so removing an entry ends that session on the next `dboss rescan`.
The app receives the signed-in email as `X-Dboss-User`; dboss strips that header from every inbound request, so the app can trust it.
A request that does not accept `text/html` gets `401` instead of a redirect.
`basic_auth` and `auth` are independent: when both are set, both must pass.

`authcog` is the app-level login service: dboss runs the whole AuthCog round trip so the app needs no AuthCog code of its own.

```yaml
authcog: true   # or a path; true captures /authcog, which must match the realm's redirect_path
```

The app links to the path. dboss mints the challenge, sends the browser to `https://<authcog_realm>/d:<host>[/p:<port>][/s:http]` (the port only when it is not the scheme's default, the scheme only when the request was not https; it is read from TLS or the edge's `X-Forwarded-Proto`), and on the `?callback=` return exchanges the one-time hash server-side. It then forwards one request to the app's own route at that path with the profile in `X-Dboss-User` (`{"email","name","avatar","provider"}`). The app reads what it needs and creates its own session; dboss keeps no session. `X-Dboss-User` is removed from every inbound request, so only dboss can set it, and it is set only on that post-login request. Any AuthCog account is admitted, and logout is the app's job. `authcog` is independent of `auth`: its login path is never gated by `auth`.

Each request walks the stages in this order: canonical redirect, `allow_ips`, health endpoint, `authcog` login, `auth` sign-in, `basic_auth`, maintenance, static files, body buffer, then wake or forward.

* A request with no or wrong credentials gets `401` at the auth stage and never reaches the wake stage, so a crawler or scanner cannot start a protected sleeping app. The first request with valid credentials wakes it.
* With no `basic_auth` any request wakes a stopped app, except an `autostart: button` app, which only its start button's POST wakes.
* The health endpoint answers before auth, so a Cloudflare health check works on a protected app. It never wakes the app.
* A pubsub publisher presenting the app's publish secret passes without the basic-auth credentials or a sign-in session.
* The same holds for `auth`: no session means no wake, the health endpoint stays open, and static files are protected.

## Static files and pages

Each web process serves `./public` straight from disk (`static` on the web procfile entry, relative to the app folder) for GET and HEAD, without waking the app.
Set `static: /path` for another directory or `static: false` to disable it for that process.
Only the common asset types in `static_extensions` are served: css, js, mjs, map, json, txt, xml, ico, images, fonts, mp4, webm, mp3, pdf, wasm and webmanifest.
A missing file, a directory, or a file with any other extension (an `.html` page, a dotfile, no extension) is a normal request to the app, so a route always wins over a stray file.
A missing `public` folder simply turns static serving off; `static_extensions: []` serves any regular file.
Paths under `static_immutable` (default `/assets/`) are cached as immutable for a year, everything else for an hour.

Every page dboss answers with itself is built in and can be replaced:

| Page | Status | Served when |
|---|---|---|
| `starting` | 503 | the app is waking up; it reloads every 5 seconds |
| `waiting` | 503 | a hand-run `dboss start` still waits for ENTER; it reloads every 5 seconds |
| `stopped` | 503 | an `autostart: button` app is stopped; it carries the start button |
| `crashed` | 503 | the app hit its restart limit |
| `maintenance` | 503 | `dboss maintenance <app> on` |
| `error` | 502/5xx | the app is unreachable, or answers 5xx (see below) |
| `forbidden` | 403 | `allow_ips` turns the visitor away |
| `signed_out` | 200 | after `/.well-known/dboss/logout` |
| `404` | 404 | a host no app owns (host only) |
| `login` | 401 | the console without a session (host only) |

For each page dboss reads, on every request, the first of `<name>.html` in the app's `pages` folder (default `./public/error_pages`), `template.html` there, the same two in the host's `pages` folder, and the built-in template. One `template.html` therefore covers every page. A page fills `{{status}}`, `{{title}}`, `{{message}}`, `{{action}}` (the start button or sign-in link dboss builds), `{{app}}` and `{{dboss_logo}}` (the dboss mark, served at `/.well-known/dboss/logo.svg` on every host) and leaves any other `{{...}}` alone.

```sh
dboss pages shop                  # which file serves each page
dboss pages dump shop             # write template.html to edit
dboss pages dump shop error       # write error.html with its wording
dboss pages dump --all            # the host template and every page
```

Only a GET that accepts `text/html` gets a page; API and non-GET requests get the bare status.
dboss's own errors (`502` when the app is unreachable, timed out or has no port) always render the `error` page.
The app's own `5xx` answers are replaced with it, status kept, only when the app's pages folder has `error.html` or `template.html`, so an app that ships no pages keeps its own error bodies.
An app's own `404`s are never touched.
`./demo/apps/sinatra` ships a `template.html` and links to each case from its front page.

## Restarts, scaling and forwarded headers

`dboss restart` (and the console button, and a `restart: true` deploy hook) **rolls** a running app with a web process.
Each web copy in turn gets a replacement started in a free port slot next to it; once the replacement passes its health check the proxy's route moves to it, the old copy leaves the route, gets `stop_timeout` for its in-flight requests, then `stop_signal`, then a kill.
A replacement that exits or never passes `health_timeout` is killed, the old copy keeps serving, `health-timeout` is posted and the restart returns the error, so a broken release never takes the app down.
Workers are stopped and started again, never run twice at once, and the `start` lifecycle step runs first.
The app stays `running` throughout; the app card shows a `rolling restart` badge.
Restarting one web process from its app card row rolls just that process or copy.
A stopped or crashed app, or one with no web process, restarts the plain way.

`count: N` on a procfile entry runs N copies (`web.1` .. `web.N` in the console, logs and metrics; one copy keeps the bare name), each with its own `PORT` and `PROC_INSTANCE`.
The proxy sends each request to the ready copy of the matched web process with the fewest requests in flight, and one copy that crashes is restarted on its own while the others keep the app serving.
A web process holds one spare port on top of its copies for the roll, so the port a copy listens on alternates between restarts.
A changed `count` applies on the next restart.

`dboss stop` (and the console button) first marks the app **draining**: the proxy answers new requests with `503` and `Retry-After`, while requests already in flight finish, bounded by `stop_timeout`. Only then does the supervisor send `stop_signal` to the process group. The app card shows a `draining` badge and `dboss ls` prints it in the state.

`deletable: true` opts an app into permanent removal through `dboss destroy <app>` or the console's **Destroy** button; the default is false.
Destroy drains and stops the app, clears its running and maintenance state, removes its entry from the apps directory and drops it from the live host.
A plain app folder is removed recursively; an app symlink is unlinked without following its target, which remains owned by lux-deploy.
Single-app mode cannot destroy itself, and retained logs, audit rows and config history continue through their normal retention.

On the way to an app the proxy adds `X-Forwarded-Proto`, `X-Forwarded-Host` and `X-Real-IP` when they are missing; whatever Cloudflare sent is left untouched. `X-Forwarded-For` is appended by the reverse proxy.

An app's processes start with the web processes (the ones with `hosts`) first, then the rest in name order, so a web process that expects other services to be up still gets that.

## Audit log

Every mutating action records who did what to which app and how it turned out: start, stop, restart, destroy, maintenance, rescan, cron runs, hook runs, `exec`, and config file writes and restores. Console actions carry the signed-in email, a hook ping carries `hook:<app>/<hook>`, and control-socket actions are attributed to `cli`.

Rows live in an `audit` table in the reserved `_dboss` database, are kept for `audit_retention` (default `8760h`, `0` keeps them forever), and are pruned with the daily log prune. The console has an **Audit** tab with app, actor and action filters; `dboss audit [--app name] [--actor who] [--action name] [-n rows]` prints the same rows.

## Config history

Every config save first copies the current file to `dir/state/config-history`, keeping the last 50 revisions per file. In the console's Configuration view the **History** button lists them; **View** shows a revision and **Restore** writes it back (revision-checked, then rescanned, and recorded in the audit log). From the CLI:

```
dboss config history [app]
dboss config restore [app] <revision>
```

Both work on the host file (no app) or one app's file. A CLI restore writes the file on disk; the running host applies it on the next `dboss rescan`.

## Management console

The console is served for `management.host` on the proxy listener and again on the first port of `ports` (`3100` in the demo), where `127.0.0.1` and `localhost` are also accepted.
A dev session (one app run from its own folder) always gets a loopback console, with or without a `management:` block, on a free port it claims from `ports`.
`dboss start` prints the loopback address first, and the public address too (`https://` on the first `management.host`):

```
management console: http://127.0.0.1:3100 (run `dboss login` for a one-time sign-in link)
management console: https://dboss.example.com (AuthCog sign-in)
```
`dboss start --login` also prints a one-time loopback sign-in link on stdout (never in the daemon log); `make demo` uses it.
The browser tab reads `<hostname> | dboss`, so consoles of several boxes stay apart.
It shows every app with state, uptime, memory, last activity and request rate, offers start, restart, stop and maintenance controls, and adds a typed-confirmation destroy action when the app sets `deletable: true`.
Next to an app's name a gray `git:<branch>` label names the branch it runs and links to it on the git host: the branch and `origin` remote of the checkout in its folder, or `GIT_BRANCH` and `GIT_REPO` from its `.env` for a packed release without `.git` (lux-deploy writes both).
It links to the process logs and edits the host and app `dboss.yaml` files in place with validation, conflict detection and a "restart required" notice for host keys that only apply on the next start.
The **Config** view has two modes: **YAML** edits the raw file, and **Form** offers a visual editor built from recipes (PubSub channels, Web, Health and runtime for an app; Notifications and the PostgreSQL connection for the host).
Each field shows a friendly label, its key, the description from the key reference and the default as a placeholder; a blank field means "use the default", so the key is removed from the file.
A form save is written to the server-only `dboss.local.yaml` next to the file (created from the base when missing), so a deploy never overwrites a value entered here.
The **Sys** tab is a read-only inspection of the box: hostname, OS and kernel, public IP, uptime, load, memory and disk use, the dboss runtime, chosen environment variables, and the installed toolchains (Go, Node, npm, Bun, Deno, Yarn, pnpm, Ruby, gem, Bundler, Python, pip, uv, PHP, Composer, Java, SQLite, lsof, rsync, curl, Docker, podman and more) with their paths and versions, each name linked to its project page.
It never starts, stops or changes anything; the `sysinfo` module keeps the snapshot warm and **Re-inspect** re-probes on demand.
The **dboss** field names the running build and **Latest release** the newest tag published on GitHub, linked to its release page and badged `up to date` or `update available`, so a box that needs `sudo dboss update` says so.
**Public IP** is the box's own routable address when it has one, which is the normal case for a server, and otherwise the address an echo service (`api.ipify.org`) sees, so a box behind NAT still reports something DNS can point at; the copy button puts it on the clipboard.
Both facts leave the box, so each is looked up at most once an hour behind a short timeout and left empty when it cannot be reached; the rest of the tab works offline.

### Signing in

A dev session does not sign in at all: a request whose peer is a loopback address is admitted as `cli@localhost`, so `management.admins` is not required and the startup line reads `(open from this machine, no sign-in)`.
The check is the connecting address and never a forwarded-for header, so a request from off-box cannot claim to be local; a reverse proxy on the same host can, which is why nginx, caddy or `cloudflared` does not belong in front of an app run this way.

Production sign-in goes through AuthCog: the console redirects to `authcog_realm`, and only the addresses in `management.admins` are admitted. A console session lasts `defaults.session_ttl`.
AuthCog sends the browser back to the address the sign-in started on, over http or https and on any port.

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

The loopback link uses the console's own listener, the first port of `ports`, so it needs no DNS.
In a host session that listener accepts `127.0.0.1` and `localhost` only for sessions created this way; without one it shows a page telling you to run `dboss login`.
The public link goes through `management.host`, so it signs in from any browser that can reach the edge.
Without a public URL, tunnel the port first: `ssh -L 3100:127.0.0.1:3100 <host>`.

### Frontend

The console is a [fez](https://github.com/dux/fez) application.
Everything lives under `./internal/console/static/` and is embedded in the binary:

* `index.html` - the SVG icon sprite and a single `<db-shell>` tag, plus one `<script fez="...">` tag per shared widget.
* `fez.min.js` - the fez runtime, copied from https://dux.github.io/fez/dist/fez.min.js.
* `app.css` - the whole stylesheet, a light Tabler-style theme; components carry no `<style>` blocks.

Every page is a hash route on `/`, so reload, Back/Forward and a pasted link all reproduce the same view:

* `fez/db-shell.fez` - navbar, the `ROUTES` list that drives it, the route outlet, API calls and the 5 second poll; exposed as `Dboss`.
* `fez/tpl-overview.fez` - `#/overview`: stat cards and the service list.
* `fez/tpl-logs.fez` - `#/logs`: the log viewer page, a thin wrapper around `db-log-view`.
* `fez/tpl-traffic.fez` - `#/traffic`: per-app requests over time, error rate, latency quantiles and the top paths, status codes, countries, client IPs and methods from the request log.
* `fez/tpl-audit.fez` - `#/audit`: operator actions with app, actor and action filters.
* `fez/tpl-sys.fez` - `#/sys`: read-only host facts, resource use and installed toolchains with versions.
* `fez/tpl-pg.fez` - `#/pg`: the PostgreSQL databases and their backups; `#/pg?db=<name>` is one database, with a Backup and a SQL tab (`&tab=sql`).
* `fez/tpl-pubsub.fez` - `#/pubsub`: one entry per hub, with its secret and a publish form.
* `fez/tpl-config.fez` - `#/config`: config file list, the YAML/Form mode toggle, the editor and revision history.
* `fez/tpl-help.fez` - `#/help`: a topic list with the operator guide and the live key reference.

The shared widgets are loaded once from `index.html` and used by several pages:

* `fez/db-log-view.fez` - the log viewer itself: left nav of apps with sqlite size and fold-out channels, time range, filters, search, row detail and export.
* `fez/db-app-card.fez` - one service: status badge, stats datagrid, actions.
* `fez/db-config-form.fez` - the visual config editor: one form per recipe, driven by `config.Recipes()`.
* `fez/db-config-keys.fez` - searchable key reference, rendered live from the key registry.
* `fez/db-preview-yaml.fez` - highlighted YAML for the examples in the Help pages.
* `fez/ui-tabs.fez` - an in-page tab strip: `tabs="backup:Backup,sql:SQL"`, the active id, and an `onselect` the page handles.
* `fez/db-toast.fez` and `fez/db-drawer.fez` - self-mounting singletons exposed as `Toast` and `Drawer`.

A new page is one `tpl-<name>.fez` plus one `ROUTES` entry; `TestEveryConsoleRouteHasTemplate` fails when a route has no template.

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
internal/secret/      generated pubsub secrets under dir/state
internal/supervisor/  process supervisor, health checks, idle stop, state files, log writer/seal
internal/ports/       fixed port allocation inside ports, listener lookup and clearing (lsof)
internal/proxy/       filter pipeline, host routing, static files, maintenance, wake, request log
internal/pages/       the built-in page template and logo, the page lookup chain and dboss pages dump
internal/authcog/     AuthCog sign-in flow shared by the console and the per-app proxy gate
internal/pubsub/      realtime channel hubs served in front of a web process
internal/schedule/    cron expression parsing and the daily HH:MM timer
internal/preview/     github_pr preview requests and app file templates
internal/git/         token-authenticated clone and reset for deploys and previews
internal/fsutil/      atomic writes for the daemon's state files
internal/httpx/       request helpers shared by the console, proxy and pubsub
internal/alerts/      error-rate and slow-request checks over the request log
internal/logstore/    per-app SQLite log store: requests, channels, FTS search, tail offsets, prune
internal/ingest/      seals stdout, tails app log files and the dboss daemon log into the store
internal/tmpclean/    daily sweep of each app's ./tmp (tmp_clean)
internal/diskusage/   daily measurement of what each app occupies on disk
internal/logx/        leveled logger for dboss's own output (log_level)
internal/sysinfo/     read-only host inspection: OS, load, memory, disks and installed toolchains
internal/pg/          PostgreSQL inspection, scheduled dumps, retention and restore
internal/metrics/     Prometheus text rendered from the app snapshots
internal/notify/      debounced operator webhook for crash and failure events
internal/version/     build version: the commit count in main, injected at build time
internal/release/     the GitHub release source: the latest tag, its URLs and tag comparison
internal/console/     management console: auth, JSON API, embedded fez frontend
internal/ctl/         control socket protocol, server and client
internal/ops/         one implementation of every app action, shared by CLI and console
internal/res/         resource backend: process groups or cgroup v2 limits
demo/                 host config and three sample apps
```

## Validation

```sh
make check                                   # go vet + staticcheck + go test ./...
go test ./internal/console/                  # console API and auth, including dboss login
bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'   # component syntax check
```
