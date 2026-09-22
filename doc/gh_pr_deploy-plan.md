# GitHub PR deploy plan

> Status: proposed, not implemented. This is the agreed design; do not build it until asked.

## Goal

Make PR previews a first-class dboss feature instead of a GitHub Actions plus SSH script.

One reserved hook, `github_pr`, handles the whole lifecycle: interpolate an app template from the request params, check out the branch, start the app, and tear it down when the PR closes. Per-preview setup and cleanup live in the template's `lifecycle` steps.

The odoo-docker repo then keeps only a tiny Action that curls dboss; no runner script, no SSH keys, no per-PR container logic.

This reverses the earlier rule that dboss has no deploy logic. From here dboss owns GitHub PR previews specifically, and the rule in AGENTS.md and README.md is updated to say so.

## Trigger

A single GitHub Action step, no SSH, keeps the existing PR comment:

```bash
curl -fsS -X POST \
  "https://dboss.staging.erpxo.rudex.hr/hooks/github_pr?action=${{ github.event.action }}&branch=${{ github.head_ref }}&repo=${{ github.event.pull_request.head.repo.clone_url }}&num=${{ github.event.number }}"
```

* `repo` is the head repo clone URL, so fork PRs work without special casing.
* `action=closed` tears the preview down. Any other action deploys or updates the branch tip.
* `num` is audit-only; naming ignores it.
* Replaces `deploy-pr.yml`, `cleanup-pr.yml` and `cleanup-orphans.yml`. No VPS SSH secrets are used for PRs.

The user sends the branch name, not a commit sha. dboss deploys the branch tip, and events for one branch are serialized so a late ping cannot race an earlier checkout.

## Config

Host-level `dboss.yaml`:

```yaml
hooks:
  github_pr:
    # secret: $GITHUB_WEBHOOK_SECRET   # optional; generated under state_dir when omitted
    repo: https://github.com/rudex/odoo-docker.git   # fallback when the request omits repo
    template:
      name: $QS_BRANCH
      hosts: [pr-$QS_BRANCH.staging.erpxo.rudex.hr]
      autostart: false
      deletable: true
      idle_stop: 6h
      lifecycle:
        create: ./bin/dboss/setup.sh
      procfile:
        web: {command: ./start.sh, health: /web/health}
```

## Semantics

* Params `action`, `branch`, `repo`, `num` become `QS_ACTION`, `QS_BRANCH`, `QS_REPO`, `QS_NUM`. A missing `branch` or `repo` fails the hook.
* Normalization happens per interpolation context:
  * The raw `QS_BRANCH` value replaces `/` with `_`.
  * Identifier fields (`name`, env values) lowercase, replace `/` with `_`, and drop characters outside `[a-z0-9_]`.
  * Host fields lowercase, replace `/` and `_` with `-`, drop characters outside `[a-z0-9-]`, and cap the label at 63 characters. A literal `_` in the template host also becomes `-`.
  * Interpolation supports `$NAME` and `${NAME}`.
* `github_pr` is reserved. Mixing it with `command:` is an error. The built-in is host-level only; an app hook named `github_pr` stays a generic command hook. Any other host hook name is a generic command hook.
* `secret` is optional. When omitted, dboss generates a 64-character secret under `state_dir/hook-secrets.json`, and `dboss hooks --host github_pr` prints the ready-made URL.
* dboss owns git. It fetches the branch tip (`git fetch` then `git reset --hard origin/<branch>`, or clone) over HTTPS, authenticated with the server's `github_token`.
* dboss does not manage the preview's databases; the template's `lifecycle` steps create what the app needs and clean it up.
* Concurrency is per branch: events for one branch queue, different branches run in parallel.
* Preview apps live in the same apps dir with the checkout inside the app dir, symlinked in like today's `deploy.sh` layout.

## Lifecycle

Deploy (`opened`, `synchronize`, `reopened`):

1. Resolve params, refuse reserved names (`main`, `development`), interpolate the template.
2. Clone the repo at the branch if the dir is missing, else fetch and reset to the branch tip.
3. Render `dboss.local.yaml` (written through the config store, so config history and a `config-write` audit row happen).
4. Symlink into the apps dir and `rescan`.
5. Start the app, which runs its `create` (first deploy) and `start` steps.

Teardown (`closed`):

1. Stop the app.
2. Remove the symlink, app dir and state.

## Audit

The `_dboss.audit` table (`ts, actor, app, action, detail, result, error`) gets one row per step, actor `hook:github_pr`, app name interpolated, detail `branch=<b> num=<n>`:

* `hook-run` is the ping itself.
* `deploy` for a successful create or update.
* `config-write` when the app file is written.
* `stop` and `destroy` on teardown.

## dboss changes

* Config (`./internal/config`): host `hooks:` (`Hooks` on `Config`, added to `hostKeys`), built-in `github_pr` validation (reserved name, required fields), the template struct, and `$VAR`/`${VAR}` interpolation with per-context sanitization. Add the block and key specs, update the reference, and revise the no-deploy-logic note.
* Endpoint (`./internal/console/console.go`): `handleHook` splits on `/`, one segment is a host hook and two is an app hook, and `/hooks/github_pr` routes to the built-in. Query params become `QS_*`.
* Engine (`./internal/preview`, new): params, interpolation, the per-branch lock map, checkout, config render, start and teardown. It depends on narrow interfaces for the runtime, the app config writer, the auditor and the log sink.
* Wiring (`./internal/daemon/daemon.go`): build the preview engine and pass it to `ops.New`, so `ops.Service.Do` dispatches `github_pr` and audit work is first-class rather than shell side effects. The config writer reuses the `apps.NewStore` semantics with config history.
* CLI and console: `dboss hooks --host <hook>` to list, run and rotate, and the Hooks panel shows host hooks.
* Tests: interpolation and sanitization table, reserved-name validation, host and app endpoint routing, checkout at the branch tip, teardown, audit rows, and per-branch serialization.

## odoo-docker changes

* `bin/dboss/setup.sh`: the per-branch `create` step, reusing `upgrade-addons.sh`, with no git.
* One workflow with the curl plus the existing PR comment. Delete the three SSH workflows.

## Rollout

1. Query params to env, host-level hooks, the split endpoint, secrets and CLI, all generic.
2. Template interpolation plus the `github_pr` checkout and start.
3. Teardown, audit, console and logs.
4. Swap the odoo-docker workflow, remove the old ones, update the docs.

## Out of scope

* `main` and `development` stay on their current deploy for now; they can move to `deploy: true` push hooks later.
* No dedup. The Action does not redeliver, and a re-run is explicit.
