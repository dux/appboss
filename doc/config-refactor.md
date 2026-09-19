# Config as a first-class citizen

## Goal

Make one config registry the single source of truth for every setting, so the key listing, the console keys view, the annotated reference and the visual form all stay in sync from it.

Every key gets a block, a section, a name, a description, a default, one or more types, and flags for enum, required, secret and per-process.

The output is clear, minimal and readable for a human and for an LLM reading the config.

The parser and validation stay on the Go structs, so paths, nesting and default values cannot drift.

## Where it stands today

Config metadata is spread across hand-maintained places.

* `./internal/config/config.go` holds the structs and `Default()`, the real parser and the source of path, type and default.
* `./internal/config/keys.go` holds `keyDocs` (description and example), the `Key` type, and three groups that are really file roles: `host`, `app` and `shared`.
* `./internal/config/recipes.go` holds `recipeSpecs`, the curated form with title, section, label and options.
* `./internal/config/reference.yaml` holds the long-form prose, embedded into the binary.
* The same three group lists are hardcoded again in `./internal/cli/cli.go` (`keyGroups`) and `./internal/console/static/fez/db-config-keys.fez` (`GROUPS`).

Adding one key today means touching the structs, `keyDocs`, `reference.yaml`, possibly `recipeSpecs`, and two group lists.

## Scope

`host` currently means file role, which collides with the block concept, so the model separates them.

The scope values are `service`, `app` and `both`.

* `service` is the root `dboss.yaml`.
* `app` is an app folder, `apps/<name>/dboss.yaml` or the single-mode folder.
* `both` is a block that appears in either file, so the app block also lives under `defaults:` in the root file.

The `host` identifiers inside `internal/apps` stay as they are, to avoid churn.

## Model

A new `./internal/config/schema.go` defines:

```go
type Scope string // "service" | "app" | "both"

const (
    ScopeService Scope = "service"
    ScopeApp     Scope = "app"
    ScopeBoth    Scope = "both"
)

type Block struct {
    ID      string
    Title   string
    Summary string
    Scope   Scope // which file role(s) the block appears in
    // display order is slice order
}

type KeySpec struct {
    Block       string   // block id
    Name        string   // human label
    Description string   // one line
    Enum        []string // allowed values -> select and validation
    Example     string
    Required    bool     // unconditional presence only
    Secret      bool
}
```

`Path`, nesting and `Default` keep coming from the structs and `Default()`.

`Types` and `PerProcess` are derived too, so each fact has one source: `typesOf(reflect.Type)` maps `List` to `string | list`, `[2]int` to `[from, to]`, `Autostart` to `bool | button` and maps to `map`, and the walk marks the process keys as per-process.

`Key.Type` stays as the display string and is `strings.Join(Types, " | ")`, so the form and the tests keep working.

A new `./internal/config/keyspecs.go` holds the roughly 100-entry `map[string]KeySpec`, absorbing `keyDocs`.

Structural map keys (`cron`, `hooks`, `processes`, `postgres.backup.databases`) stay one `KeySpec` each, and their inner fields are documented in `Description`, the same as today.

### Blocks

Service blocks:

* `paths` - apps, state_dir, log_dir, socket
* `proxy` - listen, guards, wake, upstream
* `management` - host, url, auth, metrics
* `ports` - range
* `daemon` - ticks, schedules, log level
* `notify` - webhook settings
* `postgres` - connection and backup policy
* `s3` - object storage

Blocks shared by service and app:

* `runtime` - the process keys, the per-process ones marked as such
* `web` - proxy behaviour in front of the app
* `pubsub` - realtime channels

App-only blocks:

* `app` - procfile, hosts, web_process, canonical_host, autostart
* `cron` - scheduled one-shot commands
* `hooks` - signed one-shot commands

Sections stay for form subheads, for example runtime -> Health / Restart / Resources / Logs, postgres -> Connection / Backup / Keep, and proxy -> Listener / Guards / Wake / Upstream.

## What gets derived

* `Keys()` joins the struct walk with `KeySpec` and returns `Key{Path, Block, Section, Name, Scope, Types, Type, Description, Default, Example, Enum, Required, Secret, PerProcess}`.
  The `--keys` command, `GET /api/config/keys` and the console keys view read this.
* `Blocks()` returns the ordered blocks, used by the CLI and the console instead of the hardcoded `keyGroups` and `GROUPS`.
* `RecipeField` values come from `KeySpec`: label from `Name`, options from `Enum`, widget from `Types` plus `Enum`.
  The curated `recipeSpecs` stays as the form grouping, sections and order, and drops the per-field label and options.

## What stays hand-written

* `./internal/config/reference.yaml` keeps the long-form prose.
  A new `TestReferenceCoversKeys` walks the `reference.yaml` node tree across its `---` parts, collects every key path, normalizes a leading `defaults.` (shared keys are under `defaults:` in part 1 and top-level in part 2), and checks that every `KeySpec` path appears.
  The test is one-directional, so example keys such as `cron.<name>.schedule` that are not config keys do not fail it.
  Every current leaf name already appears as a listed key, so it should pass without new prose.
* `recipeSpecs` keeps the curated sections, order and grouping.
* `required` on `KeySpec` marks only unconditional keys, in practice `procfile`.
  Conditional rules stay in `validate()`.
* `Required` and `Secret` have no runtime consumer yet; they are metadata for the key listing, an LLM reading the schema and future helpers.

## Not doing

* No generated reference and no prose migration.
* No auto one-form-per-block.
* No code-wide `host` to `service` rename.
* No `Types`, `Section` or `PerProcess` stored in `KeySpec`; they are derived.
* No `defaults.` prefix work for the form.

## Files

* `+` new `./internal/config/schema.go` - types and blocks
* `+` new `./internal/config/keyspecs.go` - the registry, content migrated from `keyDocs`
* `~` `./internal/config/keys.go` - remove `keyDocs`, extend `Key`, replace `typeName` with `typesOf`, add `Blocks()` and `Scope`, keep `formatValue`
* `~` `./internal/config/recipes.go` - source fields from `KeySpec`, keep `recipeSpecs`
* `~` `./internal/config/keys_test.go` and `./internal/config/recipes_test.go` - bidirectional spec coverage, block and scope validity, type union and widget tests
* `+` `./internal/config/reference_test.go` - node-walk coverage of `reference.yaml`
* `~` `./internal/cli/cli.go` - delete `keyGroups`, group `printKeys` by block and scope
* `~` `./internal/console/console.go` - `GET /api/config/blocks`, keys view exposes block and scope
* `~` `./internal/console/static/fez/db-config-keys.fez` - render blocks from the API, drop `GROUPS`
* `~` `./internal/console/static/fez/db-config-form.fez` - minor, the field shape is unchanged
* `~` `./internal/cli/cli_test.go` and `./internal/console/console_test.go` - update the `PART 1` reference expectation
* `~` `./README.md` and `./AGENTS.md` - config docs and the new-key workflow (one struct field plus one `KeySpec`)

The `/api/config/keys` JSON shape changes with the new `Key`, so `db-config-keys.fez`, the CLI and the tests move in the same phase.

## Verification

* `make check` for vet and tests.
* `TestKeySpecsCoverStructs` enforces coverage in both directions.
* `TestReferenceCoversKeys` keeps the reference complete.
* `bun ~/dev/gems/fez/bin/fez compile 'internal/console/static/fez/*.fez'` and a browser look at the config form and keys view.

## Phases

Each phase builds and passes tests on its own.

1. Registry plus `Keys` and `Blocks` plus tests, including the reference coverage test.
2. CLI and console keys view by block.
3. Recipe fields sourced from `KeySpec`.
4. Docs and final `make check`.
