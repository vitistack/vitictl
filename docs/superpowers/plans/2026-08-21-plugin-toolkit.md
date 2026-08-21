# Plugin Toolkit (`pkg/plugin`) Implementation Plan — step 1 of the migration

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `pkg/plugin/{picker,output,release,viticli,selfupgrade}` to vitictl — the shared toolkit every viti plugin currently duplicates — behavior-identical to the best existing copy of each.

**Architecture:** Straight code moves with minimal parameterization. Sources of truth: vitictl-talos's picker (multi-select superset), kubevirt/nhn's output (identical), nhn's release (token discovery) and viticli (failure diagnosis), nhn's upgrade/version commands (turned into constructors). No vitictl command adopts the kit in this step; plugins migrate in follow-up plans after v0.0.33 ships.

**Tech Stack:** Go, cobra, termui v3 + sahilm/fuzzy (both already in vitictl's go.mod via viti-gui), httptest for release tests.

**Spec:** `docs/superpowers/specs/2026-08-21-plugin-toolkit-design.md`

## Global Constraints

- Nothing NHN-specific may enter vitictl (no ROR, no NHN endpoints/wording).
- No behavior changes vs the source copies; only identifiers explicitly listed here change.
- Source repos are read at `/home/andreh/repo/vitictl-talos`, `/home/andreh/repo/vitictl-nhn`, `/home/andreh/repo/vitictl-kubevirt`. vitictl-talos is a colleague's repo: READ ONLY, never modify it.
- Commits are local only. Nothing is pushed; the release is gated separately at the end.
- Every task ends with `go test ./... && go build ./...` green in `/home/andreh/repo/vitictl`.
- Commit messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.

---

### Task 1: `pkg/plugin/picker` + the kit's doc.go

**Files:**
- Create: `pkg/plugin/doc.go`
- Create: `pkg/plugin/picker/model.go`, `pkg/plugin/picker/select.go` (verbatim copies from vitictl-talos)
- Test: `pkg/plugin/picker/model_test.go` (verbatim copy from vitictl-talos)

**Interfaces:**
- Consumes: nothing.
- Produces (used by Tasks 5's picker-free code not at all; used by plugin migrations later): `type Item struct{ Label string; Columns []string; Value any }`, `var ErrCancelled error`, `func Interactive() bool`, `func Select(title string, header []string, items []Item) (Item, error)`, `func SelectMulti(title string, header []string, items []Item) ([]Item, error)`.

- [ ] **Step 1: Copy the test first and watch it fail**

```bash
mkdir -p /home/andreh/repo/vitictl/pkg/plugin/picker
cp /home/andreh/repo/vitictl-talos/internal/picker/model_test.go /home/andreh/repo/vitictl/pkg/plugin/picker/
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/picker/
```
Expected: FAIL (build error — `model.go`/`select.go` missing; package contains only a test).

- [ ] **Step 2: Copy the implementation verbatim**

```bash
cp /home/andreh/repo/vitictl-talos/internal/picker/model.go \
   /home/andreh/repo/vitictl-talos/internal/picker/select.go \
   /home/andreh/repo/vitictl/pkg/plugin/picker/
```
No edits: the package is self-contained (imports only stdlib, sahilm/fuzzy, termui) and is already named `picker`.

- [ ] **Step 3: Run tests to verify they pass**

```bash
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/picker/ && go build ./...
```
Expected: PASS.

- [ ] **Step 4: Write `pkg/plugin/doc.go` (the versioning policy from the spec)**

```go
// Package plugin is the shared toolkit for viti plugins: the interactive
// picker, table/JSON/YAML output, GitHub release checks, the viti shell-out
// helper, and the self-version/upgrade command scaffolding. Plugins import
// it instead of maintaining their own copies.
//
// Compatibility: vitictl is a v0 module and this package makes NO
// compatibility promise between tags. Plugins pin an exact vitictl version
// in go.mod and upgrade deliberately; breaking changes here are called out
// in vitictl release notes.
package plugin
```

- [ ] **Step 5: Verify and commit**

```bash
cd /home/andreh/repo/vitictl && go test ./... && go vet ./... && git add pkg/plugin && \
git commit -m "feat(pkg/plugin): add shared picker (talos superset: Select + SelectMulti)" \
  -m "Verbatim move of vitictl-talos's picker — the superset of the three plugin copies (adds multi-select). First package of the shared plugin toolkit; see docs/superpowers/specs/2026-08-21-plugin-toolkit-design.md." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: `pkg/plugin/output`

**Files:**
- Create: `pkg/plugin/output/output.go` (copy from vitictl-kubevirt, one comment swapped)
- Test: `pkg/plugin/output/output_test.go` (copy from vitictl-kubevirt)

**Interfaces:**
- Consumes: nothing.
- Produces: whatever `vitictl-kubevirt/internal/output` exports today (`Format` parsing, table writer, JSON/YAML encoding) — unchanged names, unchanged signatures.

- [ ] **Step 1: Copy the test first and watch it fail**

```bash
mkdir -p /home/andreh/repo/vitictl/pkg/plugin/output
cp /home/andreh/repo/vitictl-kubevirt/internal/output/output_test.go /home/andreh/repo/vitictl/pkg/plugin/output/
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/output/
```
Expected: FAIL (build error — implementation missing).

- [ ] **Step 2: Copy the implementation; neutralize the one divergent comment**

```bash
cp /home/andreh/repo/vitictl-kubevirt/internal/output/output.go /home/andreh/repo/vitictl/pkg/plugin/output/
```
Then replace the YAML-comment block (the only text that differs between the three copies) with talos's provider-neutral wording. Edit `pkg/plugin/output/output.go`, old:
```go
// ROR's API types are tagged for JSON only, so a direct YAML encoder would
```
If that exact line is absent (the kubevirt copy already reads differently from nhn's), leave the comment as-is — kubevirt's copy has no ROR wording. Verify with:
```bash
grep -n "ROR\|NHN\|nhn" /home/andreh/repo/vitictl/pkg/plugin/output/output.go
```
Expected: no matches (Global Constraint: nothing NHN-specific).

- [ ] **Step 3: Run tests, verify pass, commit**

```bash
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/output/ && go test ./... && \
git add pkg/plugin/output && \
git commit -m "feat(pkg/plugin): add shared output (-o parsing, tables, JSON/YAML)" \
  -m "Verbatim move of the kubevirt/nhn copy (they are byte-identical)." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: `pkg/plugin/release`

**Files:**
- Create: `pkg/plugin/release/release.go` (copy from vitictl-nhn, `Repo`/`PluginName` consts removed)
- Test: `pkg/plugin/release/release_test.go` (copy from vitictl-nhn, adapted to the parameterized API)

**Interfaces:**
- Consumes: nothing.
- Produces (Task 5 depends on these exact names):
  - `type Latest struct{ Tag, Name, URL, Body string }` (json-tagged as in the source)
  - `func FetchLatest(ctx context.Context, repo string) (*Latest, error)` — unchanged
  - `func Compare(local, latestTag string) Status` — unchanged; `Status` constants `StatusUpToDate, StatusOutdated, StatusAhead, StatusDevelopment` unchanged
  - `func Token() string` — unchanged
  - `func UpgradeHint(pluginName string) string` — was `UpgradeHint()` using the `PluginName` const
  - `func ReleasesURL(repo string) string` — was `ReleasesURL()` using the `Repo` const
  - `const DefaultTimeout = 5 * time.Second`; `var githubAPIBase` stays a var so tests can point it at httptest.

- [ ] **Step 1: Copy the test first**

```bash
mkdir -p /home/andreh/repo/vitictl/pkg/plugin/release
cp /home/andreh/repo/vitictl-nhn/internal/release/release_test.go /home/andreh/repo/vitictl/pkg/plugin/release/
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/release/
```
Expected: FAIL (implementation missing).

- [ ] **Step 2: Copy the implementation and parameterize**

```bash
cp /home/andreh/repo/vitictl-nhn/internal/release/release.go /home/andreh/repo/vitictl/pkg/plugin/release/
```
Apply exactly these edits to `pkg/plugin/release/release.go`:

1. Delete the two plugin-specific constants and their doc comments:
```go
// Repo is the GitHub owner/name that hosts viti-nhn releases.
const Repo = "vitistack/vitictl-nhn"

// PluginName is how vitictl's plugin manager knows this plugin: the binary
// is viti-nhn, so viti exposes it as "nhn".
const PluginName = "nhn"
```

2. Replace `UpgradeHint` (keep its existing doc comment about shipping no installer, reworded plugin-neutrally):
```go
// UpgradeHint returns the command that upgrades the named plugin. Plugins
// deliberately ship no installer of their own: `viti plugin upgrade` already
// resolves the release, verifies its SHA-256 checksum and Sigstore
// signature, and replaces the binary atomically — reimplementing that in
// every plugin would mean N copies of the same security-critical code.
func UpgradeHint(pluginName string) string {
	return "viti plugin upgrade " + pluginName
}
```

3. Replace `ReleasesURL`:
```go
// ReleasesURL returns the human-readable releases page for repo.
func ReleasesURL(repo string) string {
	return fmt.Sprintf("https://github.com/%s/releases", repo)
}
```

4. Update the package doc comment's first line from "the latest published viti-nhn release" to "the latest published release of a viti plugin", and reword the private-repo paragraph to name no specific repo (e.g. "Some plugin repositories are private, and GitHub answers unauthenticated requests for private resources with 404 …").

5. `grep -n "nhn\|NHN" pkg/plugin/release/release.go` — expected: no matches.

- [ ] **Step 3: Adapt the test file**

In `pkg/plugin/release/release_test.go`, update every call site of the changed API:
- `release.Repo` / bare `Repo` as a fetch argument → a literal `"vitistack/example"` (the tests run against httptest via `githubAPIBase`, so the repo string is opaque).
- `UpgradeHint()` → `UpgradeHint("example")`, asserting `"viti plugin upgrade example"`.
- `ReleasesURL()` → `ReleasesURL("vitistack/example")`, asserting the formatted URL.
- Any test asserting the deleted constants' values: delete that test case.

- [ ] **Step 4: Run tests, verify pass, commit**

```bash
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/release/ && go test ./... && \
git add pkg/plugin/release && \
git commit -m "feat(pkg/plugin): add shared release checks (repo/plugin as parameters)" \
  -m "Moved from vitictl-nhn (the copy with private-repo token discovery). The Repo and PluginName constants become parameters: UpgradeHint(pluginName), ReleasesURL(repo); FetchLatest already took the repo." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: `pkg/plugin/viticli`

**Files:**
- Create: `pkg/plugin/viticli/viticli.go` (adapted from vitictl-nhn — the diagnosis superset)
- Test: `pkg/plugin/viticli/viticli_test.go` (ported from vitictl-nhn's Run/diagnosis tests)

**Interfaces:**
- Consumes: nothing.
- Produces (plugin migrations depend on these exact names):
```go
var Binary = "viti"                       // tests point this at a stub
var ErrNotInstalled error                 // viti missing from PATH
var ErrChildFailed error                  // child already reported; caller's root prints nothing more
type Streams struct{ In io.Reader; Out, Err io.Writer }
func Path() (string, error)
// Run executes `viti args...` attached to the caller's streams. diagnose is
// consulted only when the child exits non-zero normally; nil means every
// such failure is ErrChildFailed. Cancelled contexts and signal deaths are
// classified before diagnose is reached (silent / loud respectively).
func Run(ctx context.Context, s Streams, args []string, diagnose DiagnoseFunc) error
type DiagnoseFunc func(ctx context.Context, bin string, childErr error) error
// PluginDiagnosis distinguishes "plugin too old" (upgrade hint), "plugin not
// installed" (install hint), and "real failure" (ErrChildFailed) by probing
// --help at each level of probe; probe[0] is the plugin name, e.g.
// []string{"kubevirt", "vm", "changemachineclass"}.
func PluginDiagnosis(probe []string) DiagnoseFunc
```

- [ ] **Step 1: Port the tests first**

Copy `vitictl-nhn/internal/viticli/viticli_test.go` to `pkg/plugin/viticli/viticli_test.go`, then adapt:
- Drop the `TestArgs*` tests (the `Args(ChangeMachineClass)` builder stays in vitictl-nhn).
- Every `Run(ctx, streams, ChangeMachineClass{...})` call becomes
  `Run(ctx, streams, []string{"kubevirt", "vm", "changemachineclass", "web-1", "--yes"}, PluginDiagnosis([]string{"kubevirt", "vm", "changemachineclass"}))` (args matching what the old struct produced for that test).
- `TestPathReportsAMissingViti`, `TestRunExecutesTheBinary`, `TestRunHintsAtUpgradeWhenTheSubcommandIsMissing`, `TestRunHintsAtInstallWhenThePluginIsMissing`, `TestRunDoesNotHintAtUpgradeForOrdinaryFailures`, `TestRunOrdinaryFailureIsMarkedChildFailed`, `TestRunUpgradeHintIsNotMarkedChildFailed`, `TestRunCancelledContextIsNotMisdiagnosed`, `TestRunChildKilledWithoutReportingIsLoud` all port with their stub scripts unchanged.
- Add one new test: `Run` with `diagnose == nil` and a plainly failing stub returns `ErrChildFailed`:

```go
// A caller with no diagnosis (shelling out to a built-in viti command, like
// kubevirt's Talos-dashboard delegation) gets the plain classification:
// normal non-zero exits are the child's own report.
func TestRunWithoutDiagnosisMarksNormalFailuresChildFailed(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "viti-stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := Binary
	Binary = stub
	defer func() { Binary = orig }()

	err := Run(context.Background(),
		Streams{In: strings.NewReader(""), Out: &strings.Builder{}, Err: &strings.Builder{}},
		[]string{"machine", "console", "web-1"}, nil)
	if !errors.Is(err, ErrChildFailed) {
		t.Fatalf("err = %v, want ErrChildFailed", err)
	}
}
```

Run: `go test ./pkg/plugin/viticli/` — Expected: FAIL (implementation missing).

- [ ] **Step 2: Write the implementation**

Copy `vitictl-nhn/internal/viticli/viticli.go` to `pkg/plugin/viticli/viticli.go`, then apply exactly:

1. Package comment: replace the nhn/changemachineclass-specific paragraphs with:
```go
// Package viticli shells out to the parent viti CLI on behalf of a plugin.
//
// Functionality a plugin needs from viti (or from another plugin) is driven
// through the real command rather than reimplemented — one implementation,
// in the repository that owns it. This package owns the exec plumbing and
// the failure classification: user cancels stay quiet, children that died
// without reporting are loud, and cobra's bare "unknown command" is turned
// into an install-or-upgrade hint by PluginDiagnosis.
```
2. Delete `type ChangeMachineClass`, `func Args`, and the `changemachineclass`-specific mentions in `Path()`'s error (new wording: `"%w — install viti and the plugin this command drives"`).
3. `Run` becomes:
```go
type DiagnoseFunc func(ctx context.Context, bin string, childErr error) error

func Run(ctx context.Context, s Streams, args []string, diagnose DiagnoseFunc) error {
	bin, err := Path()
	if err != nil {
		return err
	}
	// #nosec G204 -- bin is resolved from PATH; args are a fixed subcommand
	// plus flag values, never a shell string.
	c := exec.CommandContext(ctx, bin, args...)
	c.Stdin = s.In
	c.Stdout = s.Out
	c.Stderr = s.Err
	if c.Stdin == nil {
		c.Stdin = os.Stdin
	}
	if err := c.Run(); err != nil {
		return classify(ctx, bin, err, diagnose)
	}
	return nil
}
```
4. Rename today's `explainFailure` to `classify(ctx, bin, childErr, diagnose)`; its cancelled-context and not-Exited branches stay verbatim; the trailing probe `switch` moves into `PluginDiagnosis` and `classify` ends with:
```go
	if diagnose != nil {
		return diagnose(ctx, bin, childErr)
	}
	return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
```
5. `PluginDiagnosis` wraps the moved switch, generalized on `probe` (today's hardcoded `"kubevirt", "vm", "changemachineclass"` / `"kubevirt"` / plugin name `kubevirt` become `probe...`, `probe[0]`, and `probe[0]` respectively):
```go
func PluginDiagnosis(probe []string) DiagnoseFunc {
	return func(ctx context.Context, bin string, childErr error) error {
		sub := strings.Join(probe[1:], " ")
		switch {
		case probeOK(ctx, bin, append(append([]string{}, probe...), "--help")...):
			return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
		case probeOK(ctx, bin, probe[0], "--help"):
			return fmt.Errorf(
				"your viti %[1]s plugin does not know '%[2]s' yet — "+
					"upgrade it with 'viti plugin upgrade %[1]s' (or 'viti %[1]s upgrade'): %[3]v",
				probe[0], sub, childErr)
		case probeOK(ctx, bin, "--help"):
			return fmt.Errorf(
				"the viti %[1]s plugin is not installed — "+
					"install it with 'viti plugin install %[1]s': %[2]v", probe[0], childErr)
		default:
			return fmt.Errorf("%w: %v", ErrChildFailed, childErr)
		}
	}
}
```
6. `probeOK` stays verbatim.

- [ ] **Step 3: Run tests, verify pass, commit**

```bash
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/viticli/ && go test ./... && \
git add pkg/plugin/viticli && \
git commit -m "feat(pkg/plugin): add shared viticli (exec + child-failure diagnosis)" \
  -m "Moved from vitictl-nhn — the superset with the layered --help probing. Run takes the arg list plus an optional DiagnoseFunc; PluginDiagnosis generalizes the install-vs-upgrade hinting on a probe subcommand." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: `pkg/plugin/selfupgrade`

**Files:**
- Create: `pkg/plugin/selfupgrade/selfupgrade.go` (constructors adapted from vitictl-nhn's `cmd/upgrade.go` + `cmd/version.go` + `confirm`)
- Test: `pkg/plugin/selfupgrade/selfupgrade_test.go` (ported from vitictl-nhn's `cmd/upgrade_test.go` + `cmd/version_test.go`)

**Interfaces:**
- Consumes: `pkg/plugin/release` exactly as produced by Task 3 (`FetchLatest(ctx, repo)`, `Compare`, `Status*`, `UpgradeHint(name)`, `ReleasesURL(repo)`, `Latest`).
- Produces (plugin migrations depend on these exact names):
```go
type Options struct {
	Name    string // plugin name in viti's index, e.g. "nhn"
	Repo    string // GitHub owner/name, e.g. "vitistack/vitictl-nhn"
	Version string // ldflags-injected build version, e.g. "v0.1.1"
}
func NewVersionCmd(o Options) *cobra.Command
func NewUpgradeCmd(o Options) *cobra.Command
func Confirm(cmd *cobra.Command, prompt string) (bool, error)
```

- [ ] **Step 1: Port the tests first**

Copy nhn's `cmd/upgrade_test.go` and `cmd/version_test.go` into one `pkg/plugin/selfupgrade/selfupgrade_test.go` (package `selfupgrade`), adapting:
- Tests that ran the command through nhn's root (`run(t, "version")`-style) instead build the command directly: `cmd := NewVersionCmd(Options{Name: "example", Repo: "vitistack/example", Version: "v1.2.3"})`, wire `SetOut/SetErr/SetIn`, `cmd.SetArgs([...])`, `cmd.Execute()`.
- `confirm` tests port against exported `Confirm`.
- String assertions on "viti-nhn" become "viti-example" (derived as `"viti-" + o.Name`).
- Where the old tests stubbed the GitHub API via the release package's `githubAPIBase`: that var is unexported in another package now, so those tests port only if they already went through an exported seam; tests that cannot reach the seam are replaced by tests of the pure pieces (`printReleaseStatus` equivalent, exported here as unexported func with direct test). Port what compiles; drop nothing silently — list any dropped test in the commit message.

Run: `go test ./pkg/plugin/selfupgrade/` — Expected: FAIL (implementation missing).

- [ ] **Step 2: Write the implementation**

`pkg/plugin/selfupgrade/selfupgrade.go` — nhn's two files merged and parameterized. The full transformation from nhn's sources (reproduce the source bodies verbatim except for these substitutions):
- `version` (package var) → `o.Version`
- `"viti-nhn"` → `"viti-" + o.Name`; `"viti nhn upgrade"` → `fmt.Sprintf("viti %s upgrade", o.Name)`
- `release.FetchLatest(ctx, release.Repo)` → `release.FetchLatest(ctx, o.Repo)`
- `release.UpgradeHint()` → `release.UpgradeHint(o.Name)`
- `release.PluginName` in `runPluginUpgrade` → `o.Name`
- The nhn-specific "private repository, so … GH_TOKEN" paragraphs in Long become one neutral sentence: `"If the plugin's repository is private, the check needs a GitHub token: set GH_TOKEN (or GITHUB_TOKEN), or run \"gh auth login\"."`
- `confirm` → exported `Confirm`, body verbatim.
- Import `"github.com/vitistack/vitictl/pkg/plugin/release"`.

Skeleton (bodies verbatim from source with the substitutions above):
```go
// Package selfupgrade provides the version and upgrade commands every viti
// plugin ships: `viti <name> version [--check]` and
// `viti <name> upgrade [--run] [--yes]`, delegating actual binary
// replacement to `viti plugin upgrade <name>`.
package selfupgrade

type Options struct {
	Name    string
	Repo    string
	Version string
}

func NewVersionCmd(o Options) *cobra.Command { /* nhn cmd/version.go newVersionCmd body */ }
func NewUpgradeCmd(o Options) *cobra.Command { /* nhn cmd/upgrade.go newUpgradeCmd body */ }
func Confirm(cmd *cobra.Command, prompt string) (bool, error) { /* nhn confirm body */ }
func printReleaseCheck(ctx context.Context, out io.Writer, o Options) error { /* nhn body, o.Repo */ }
func printReleaseStatus(out io.Writer, o Options, latest *release.Latest) error { /* nhn body */ }
func runPluginUpgrade(cmd *cobra.Command, o Options) error { /* nhn body, o.Name */ }
```
(The `/* … */` markers here mean: paste the exact source body and apply only the listed substitutions — no other rewording, no behavior change.)

- [ ] **Step 3: Run tests, verify pass, commit**

```bash
cd /home/andreh/repo/vitictl && go test ./pkg/plugin/selfupgrade/ && go test ./... && \
git add pkg/plugin/selfupgrade && \
git commit -m "feat(pkg/plugin): add selfupgrade — the version/upgrade commands every plugin ships" \
  -m "Moved from vitictl-nhn and parameterized on Options{Name, Repo, Version}. Confirm is exported for plugins' own prompts." \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Final verification and the release gate

**Files:**
- Modify: `README.md` (one paragraph in the plugin section pointing plugin authors at `pkg/plugin`)

- [ ] **Step 1: Full verification**

```bash
cd /home/andreh/repo/vitictl && go test ./... && go vet ./... && make lint && make build
```
Expected: all green, lint 0 issues.

- [ ] **Step 2: README paragraph**

Append to the "Publishing a plugin" section:
```markdown
Plugin authors: the shared toolkit lives at
`github.com/vitistack/vitictl/pkg/plugin` — the interactive picker, `-o`
output, release checks, the viti shell-out helper, and the standard
`version`/`upgrade` commands. Pin an exact vitictl version; the package
makes no compatibility promise between v0 tags.
```

- [ ] **Step 3: Commit the README, then STOP**

```bash
git add README.md && git commit -m "docs: point plugin authors at pkg/plugin" \
  -m "Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```
Then present the release gate for **v0.0.33** (tag + push) to the user. Do NOT push without an explicit go. After v0.0.33 ships, the follow-up plans (vitictl-kubevirt v0.1.5, vitictl-nhn v0.1.2 migrations) become unblocked.
