# Plugin toolkit: shared plugin code moves into vitictl `pkg/plugin`

**Date:** 2026-08-21
**Status:** approved (Andre, 2026-08-21)

## Problem

Every viti plugin re-implements the same toolkit. A file-level survey across
vitictl-kubevirt, vitictl-nhn, and vitictl-talos found:

- `internal/picker` — byte-identical in kubevirt/nhn; talos extended it with
  multi-select (`SelectMulti`, marker gutter).
- `internal/output` — identical except one comment.
- `internal/release` — identical except the `Repo` constant and comments;
  includes the private-repo token discovery.
- `cmd/upgrade.go`, `cmd/version.go`, `confirm()` — differ only in plugin
  name/repo.
- `internal/viticli` — kubevirt's is a subset of nhn's (which carries the
  child-failure diagnosis).

That is ~1,200 lines maintained three times, already drifting (the talos
copies diverged within days), and every future plugin starts by copying it
again.

## Decision

One implementation, hosted in **vitictl** under `pkg/plugin/...`
(import path `github.com/vitistack/vitictl/pkg/plugin/<name>`).

Why vitictl and not a separate kit repo: the kit's contract *is* vitictl
(plugins are discovered by it, dispatched by it, and `viticli` shells out to
it); the org treats CLI + plugins as one product, so independent kit
versioning buys little; and a fourth repo means another pipeline, signing
identity, and release cadence. The dependency cost of importing vitictl is
modest under Go module graph pruning, and the k8s-heavy plugins carry those
dependencies anyway.

**Boundary rule:** nothing NHN-specific enters vitictl. ROR integration, NHN
endpoints, and NHN conventions stay in vitictl-nhn.

## Package surface

### `pkg/plugin/picker`

Source of truth: **vitictl-talos** (the superset). Exports as today:
`Item{Label, Columns, Value}`, `ErrCancelled`, `Interactive()`,
`Select(title string, headers []string, items []Item) (Item, error)`, plus
talos's `SelectMulti`. `Select`'s signature is unchanged, so kubevirt/nhn
call sites migrate by import-path swap alone.

### `pkg/plugin/output`

Source of truth: kubevirt/nhn (identical). The one divergent comment adopts
talos's provider-neutral wording.

### `pkg/plugin/release`

Source of truth: nhn (token discovery included). The package-level
`Repo` constant is removed; every function takes the repo:

```go
func FetchLatest(ctx context.Context, repo string) (*Latest, error)
func Compare(local, latestTag string) Status
```

### `pkg/plugin/selfupgrade`

The `cmd/upgrade.go` + `cmd/version.go` twins become constructors:

```go
type Options struct {
    Name    string // plugin name in viti's index, e.g. "kubevirt"
    Repo    string // GitHub owner/name, e.g. "vitistack/vitictl-kubevirt"
    Version string // ldflags-injected build version
    Commit  string
}
func NewVersionCmd(o Options) *cobra.Command
func NewUpgradeCmd(o Options) *cobra.Command
func Confirm(cmd *cobra.Command, prompt string) (bool, error)
```

Semantics are exactly today's plugin behavior: `version --check` exits 0 on
an unreachable GitHub, `upgrade` exits non-zero; `upgrade --run` delegates to
`viti plugin upgrade <name>`; confirmation refuses non-terminal stdin.
(Whether plugin `upgrade` should also become upgrade-by-default like viti's
own is a separate, later decision — this move changes no behavior.)

### `pkg/plugin/viticli`

Source of truth: **nhn** (the child-failure diagnosis: probe layering,
install-vs-upgrade hints, cancelled-context and killed-child handling). The
hardcoded subcommand is parameterized:

```go
var Binary = "viti" // tests point this at a stub
var ErrNotInstalled, ErrChildFailed error
type Streams struct{ In io.Reader; Out, Err io.Writer }
// Run executes `viti <args...>` attached to the caller's streams. probe is
// the subcommand whose existence distinguishes "old plugin" from "real
// failure", e.g. []string{"kubevirt", "vm", "changemachineclass"}.
func Run(ctx context.Context, s Streams, probe []string, args []string) error
```

Domain arg-builders (nhn's `Args(ChangeMachineClass)`) stay in their plugins.

## Versioning policy

vitictl stays a v0 module: `pkg/plugin` carries **no compatibility
promise between tags**. Plugins pin an exact vitictl version in go.mod and
upgrade deliberately; breaking `pkg/plugin` changes are called out in
vitictl release notes. This is written into `pkg/plugin/doc.go`.

## Migration plan (each step its own release, behavior-identical by test)

1. **vitictl v0.0.33** — add `pkg/plugin/*` with the moved code and their
   existing tests (picker tests: talos's + kubevirt's merged). vitictl's own
   commands do not adopt the kit in this step.
2. **vitictl-kubevirt v0.1.5** — replace `internal/picker`, `internal/output`,
   `internal/release`, `internal/viticli`, and the upgrade/version commands
   with kit imports; delete the duplicated code. Existing cmd-level tests
   (help text, flags, behavior) act as the lock; full suite + lint green.
3. **vitictl-nhn v0.1.2** — same swap.
4. **vitictl-talos** — a PR offered to its owner (colleague's repo, their
   call). Until merged, talos simply keeps its copies.

## Testing

- Moved packages bring their unit tests into vitictl.
- Each plugin migration relies on its existing cmd-level tests as a behavior
  lock; tests that duplicated the moved package tests are deleted with the
  code they tested.
- Per-step verification: `go test ./...`, `golangci-lint`, `go build`, and a
  manual `--help` comparison for the upgrade/version commands.

## Non-goals

- Tier-2 sharing (`internal/kube` connect helpers) — follow-up.
- Replacing the termui picker with an inline (bubbletea/huh) renderer — a
  separate initiative; doing it after this move means writing it once.
- Migrating vitictl's own `internal/printer`/commands onto the kit.
- Any behavior change in any plugin.

## Risks

- **API discipline:** `pkg/` makes vitictl a library; mitigated by the v0
  pinning policy above.
- **Dependency coupling:** plugins inherit vitictl's module requirements;
  accepted (graph pruning keeps it modest; the heavy deps are already
  present in the plugins).
- **talos divergence:** until the talos PR lands, `SelectMulti` exists in two
  places; the kit copy is canonical from step 1 on.
