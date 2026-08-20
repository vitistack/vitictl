# vitictl

Command-line tool for Vitistack. The binary is installed as `viti`. A
Vitistack deployment can span several Kubernetes clusters, modelled as
**availability zones**: every command aggregates across all configured zones,
with `-z/--availabilityzone` (or `--az`) to narrow to one.

## Install

### One-liner (Linux / macOS)

Downloads the latest release, verifies the SHA-256 checksum and (if
[cosign](https://docs.sigstore.dev/cosign/installation/) is installed) the
Sigstore keyless signature, then installs `viti` to `/usr/local/bin` (or
`$HOME/.local/bin` when not root):

```
curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash
```

Pin a specific version, install the `viti-gui` TUI plugin alongside, or
change the install directory:

```
curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash -s -- --version v0.2.0
curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash -s -- --with-gui
curl -fsSL https://raw.githubusercontent.com/vitistack/vitictl/main/install.sh | bash -s -- --prefix "$HOME/.local/bin"
```

Run `./install.sh --help` for all flags (including `--skip-cosign` and
`--skip-checksum`).

### One-liner (Windows, PowerShell)

Installs `viti.exe` to `%LOCALAPPDATA%\Programs\viti` and appends that
directory to the user `PATH` (open a new terminal after install). SHA-256
is always verified; cosign signature is verified if `cosign.exe` is on
`PATH`.

```powershell
irm https://raw.githubusercontent.com/vitistack/vitictl/main/install.ps1 | iex
```

Pin a version, install the `viti-gui` TUI plugin alongside, or override the
install prefix:

```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/vitistack/vitictl/main/install.ps1))) -Version v0.2.0
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/vitistack/vitictl/main/install.ps1))) -WithGui
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/vitistack/vitictl/main/install.ps1))) -Prefix "$env:USERPROFILE\bin"
```

Available parameters: `-Version`, `-Prefix`, `-WithGui`, `-SkipCosign`,
`-SkipChecksum`, `-NoPathUpdate`. See `Get-Help .\install.ps1 -Full` after
downloading.

### From source

```
make install
```

## Configure

Settings live in `~/.vitistack/ctl.config.yaml`:

```yaml
availabilityzones:
  - name: prod-west
    kubeconfig: /Users/me/.kube/prod-west-config
    context: prod-west
  - name: dev
    context: dev-ctx     # uses the default kubeconfig
```

Each availability zone must supply at least one of `kubeconfig` or `context`.
An empty `kubeconfig` falls back to `$KUBECONFIG` or `~/.kube/config`; an
empty `context` uses the kubeconfig's current-context.

Every command that talks to a cluster verifies that the required Vitistack
CRDs (`vitistacks`, `kubernetesclusters`, `machines`) are installed.

### Managing availability zones

```
viti config init                                            # interactive
viti config add prod-west --kubeconfig ~/.kube/prod --context prod-west
viti config add dev --context dev-ctx
viti config list
viti config remove dev
```

## Commands

All resource commands accept `-z/--availabilityzone <name>` (or `--az
<name>`) to restrict to a single configured zone. `list`, `get`, and
`search` accept `-o/--output <format>` à la kubectl:

| `-o` value | effect                                                      |
|------------|-------------------------------------------------------------|
| (default)  | table (`list`/`search`) or emoji detail view (`get`)         |
| `wide`     | table with extra columns                                     |
| `json`     | single object or a k8s-style `List` envelope in JSON         |
| `yaml`     | same, in YAML                                                |
| `name`     | one identifier per line (`kind/namespace/name`)              |

All `list` and `search` commands print an `AZ` column by default.

`list` and `search` also accept `-s/--sort <spec>` — a comma-separated
list of columns, with a `-` prefix for descending order. Built-in keys are
`name`, `az`, `age`, and (for namespaced resources) `namespace`; each CRD
adds its own keys (e.g. `phase`, `provider`, `cluster-id`). Run a command
with `--help` to see the available keys for that resource. On `search`,
`--sort` overrides fuzzy ranking.

```
viti machine list --sort az,-age
viti kc search prod -s phase,name
```

### Vitistack

```
viti vitistack list [-o wide|json|yaml|name]
viti vitistack get <name> [-o wide|json|yaml|name]
```

### Machines (alias: `m`)

```
viti machine list     [-n namespace] [-o ...]
viti machine get <name> [-n namespace] [-o ...]
viti machine search [query] [-n namespace] [-o ...]
viti m list --az prod-west -o wide
```

### Kubernetes clusters (alias: `kc`)

```
viti kubernetescluster list     [-n namespace] [-o ...]
viti kubernetescluster get <name> [-n namespace] [-o ...]
viti kubernetescluster search [query] [-n namespace] [-o ...]
viti kc list -o yaml

# Extract cluster config artifacts (output dir defaults to ./<clusterId>).
# Here -o is an output directory (not a format) since get-config writes files.
# Disambiguate with --az and/or -n/--namespace if the name exists on multiple
# availability zones.
viti kc get-config <name> [--az zone] [-n namespace] [-o ./out]

# Install the cluster's kubeconfig + talosconfig as a <clusterId> context
# in ~/.kube/config and ~/.talos/config. Endpoints for talosconfig are
# resolved from the ControlPlaneVirtualSharedIP CR (or the -ctp machines)
# by default; use --endpoint-from secret or --endpoint <addr> to override.
viti kc login <name> [--az zone] [-n namespace] [--endpoint <addr>...] [--force] [--no-activate]

# Write kubeconfig-<clusterId> / talosconfig-<clusterId> into a directory
# instead of merging into your default configs:
viti kc login <name> -o ./out

# Provider-native dashboard. Talos → talosctl dashboard against the
# control planes with a temporary talosconfig (no prior `login` needed).
viti kc console <name> [--az zone] [-n namespace] [--endpoint <addr>...]

# Take an etcd snapshot. -o is required: a directory gets the default
# filename appended ("etcd-backup-<clusterId>.snapshot"), anything else is
# used as the literal file path. --copy-raw uses the unhealthy-cluster
# fallback (talosctl cp /var/lib/etcd/member/snap/db).
viti kc etcd-backup <name> -o ./backups/ [--node <addr>] [--endpoint <addr>...]
viti kc etcd-backup <name> -o ./snap.bin --copy-raw

# Restore etcd from a snapshot (DESTRUCTIVE — see Talos disaster-recovery
# preconditions). Adds --recover-skip-hash-check via --skip-hash-check
# when the snapshot was taken with --copy-raw.
viti kc etcd-restore <name> --from ./snap.bin [--node <addr>] [--yes] [--skip-hash-check]
```

### Decommissioning a cluster (`kc delete`)

`kc delete` decommissions a guest cluster in two phases. Phase 1 cleans up
*inside* the guest so external systems release what they are holding —
ArgoCD is stopped so nothing self-heals mid-teardown, ingresses, Gateways and
LoadBalancer services are deleted so the IPAM and DNS operators release
addresses and records, PVCs are deleted and waited on until the CSI driver has
removed the real volumes, and the cluster is deregistered from ROR. Phase 2
runs **only if phase 1 is verifiably clean**, and deletes the
KubernetesCluster CR, then watches the operator tear down the VMs, network
configuration, API VIP and node-IP allocations until they are verifiably gone.

The ordering is load-bearing: the external cleanup is performed by operators
*inside* the guest and by its CSI drivers, so it can only happen while the
guest is still alive. Once the VMs are gone, anything phase 1 missed is
leaked with no way left to find it.

```
# Check everything a real run needs, change nothing. Verifies the CR and its
# machines, that the guest API is reachable via the kubeconfig in the
# cluster's own secret, and the ROR identity and plugin. Exits non-zero if
# any prerequisite is missing.
viti kc delete <name> --dry-run [-n namespace] [-z zone]

# The real thing (IRREVERSIBLE). Prompts for the cluster name to confirm.
viti kc delete <name> [-n namespace] [-z zone] [--machine-timeout 15m] [--yes]

# Phase 1 only: clean up the guest and deregister from ROR, leaving the
# cluster and its VMs untouched — for when the irreversible part happens
# later, e.g. in a change window. Re-runnable.
viti kc preclean <name> [-n namespace] [-z zone] [--yes]

# Finish a staged decommission (or delete a guest that is already gone /
# unreachable). Skips phase 1 entirely, so external state held by the guest
# is NOT cleaned up — IPAM addresses, DNS records and volumes are leaked.
viti kc delete <name> --skip-preclean
```

Deriving guest access from the cluster being deleted makes a wrong-cluster
pairing impossible: the kubeconfig comes from that CR's own secret, never from
your current kubectl context. Every configured availability zone must be
reachable for the target to be resolved, so a same-named cluster on an
unreachable zone cannot go unseen; scope the command with `-z` if a zone is
down and you are certain which one you mean.

ROR deregistration is delegated to the
[`viti-nhn` plugin](#extensions--plugins). On a cluster with the `nhn-ror`
namespace present, a missing plugin blocks the clean verdict rather than
silently skipping the purge; clusters without that namespace are unaffected.

**Finalizer contract:** finalizers are never stripped — they *are* the
teardown mechanism, and removing one makes the object disappear while the VM,
volume or address it represents stays allocated. If a wait times out the run
is reported NOT CLEAN and stops: investigate the vitistack operators on the
management cluster rather than forcing the objects away.

### Machines (alias: `m`) — dashboard

```
# Per-node Talos dashboard. The owning cluster is inferred from the
# machine name (<clusterId>-ctp<N> / <clusterId>-wrk<N>).
viti machine console <name> [--az zone] [-n namespace]
```

### Network namespaces (alias: `nn`)

Beyond the shared `list` / `get` / `search` verbs, `nn` has two housekeeping
commands. A NetworkNamespace holds external network state (VLAN, IPv4/IPv6
prefixes, egress IP) in NAM, so both are built around the operator's
finalizer.

```
# Read-only fleet audit: NetworkNamespaces no KubernetesCluster references.
# Columns preview the delete gates (NC-REFS, IPALLOCS, GHOST-ASSOC). Reports
# how many availability zones were actually audited, and exits non-zero if
# any could not be — partial coverage is never reported as a clean fleet.
viti nn orphans [-n namespace] [--zone-timeout 30s]

# Delete one UNUSED NetworkNamespace (DESTRUCTIVE, irreversible).
viti nn delete <name> [-n namespace] [--yes] [--timeout 2m]
```

`nn delete` refuses — with no override — if a KubernetesCluster references
the namespace, a NetworkConfiguration is still bound to it (by name or
`vlan<id>` interface), or IPAllocations referencing it remain. The stale
`status.associatedKubernetesClusterIds` summary is displayed but never gates
anything. Every configured availability zone must be reachable, so a
same-named namespace elsewhere cannot go unseen, and the gates are re-checked
after confirmation.

**Finalizer contract:** deletion is delete-and-wait. The operator tears down
the external NAM state and signals completion by releasing
`networknamespace.vitistack.io/finalizer` — the object disappearing is the
proof that teardown happened. `viti` never strips or patches finalizers. If
the wait times out, the result is reported NOT CLEAN and the external state is
still allocated: investigate the networknamespace operator on the management
cluster rather than forcing the object away.

### Other CRDs

The remaining Vitistack CRDs share the same `list` / `get <name>` / `search
[query]` pattern with `-o` support (`networknamespace` adds the two commands
above). Each has short aliases:

| Command                      | Aliases               | Scope      | Emoji |
|------------------------------|-----------------------|------------|-------|
| `machineprovider`            | `mp`                  | cluster    | 🏭    |
| `kubernetesprovider`         | `kp`                  | cluster    | ☁️    |
| `machineclass`               | `mc`                  | cluster    | 🧩    |
| `kubevirtconfig`             | `kvc`                 | cluster    | 💻    |
| `proxmoxconfig`              | `pxc`                 | cluster    | 🔌    |
| `networknamespace`           | `nn`                  | namespaced | 🕸️    |
| `networkconfiguration`       | `nc`                  | namespaced | 🌐    |
| `controlplanevirtualsharedip`| `lb`, `cpvip`         | namespaced | 🧷    |
| `etcdbackup`                 | `eb`                  | namespaced | 💾    |
| `clusterstorage`             | `cls`                 | namespaced | 🗄️    |

Example: `viti mp list -o wide`, `viti eb search prod`, `viti nc get
my-nc -n my-ns -o yaml`.

**Talos** output:
- `worker.yaml`
- `controlplane.yaml`
- `secret.yaml` (from the `secrets.bundle` key)
- `talosconfig`
- `kubeconfig` (from the `kube.config` key)
- `info.txt` with every other key in the cluster secret

**AKS** output:
- `kubeconfig` (from the `kube.config` key)
- `info.txt` with every other key in the cluster secret

## TUI (`viti gui`)

`viti-gui` is a terminal UI shipped as a plugin. Install it with `--with-gui`
(`-WithGui` on Windows) or run `make build-gui` from source, then invoke:

```
viti gui
```

The first menu entry, **Secrets**, lists every `KubernetesCluster` across your
configured availability zones. Type to fuzzy-search, arrow keys to pick
(PgUp/PgDn to jump a page), Enter to open. On the detail view you can walk the secret's keys (↑/↓),
toggle base64 decoding of the current value (`b`), or show every key at once
(`a`). Esc backs out to the picker or the menu; `q` quits.

## Extensions / plugins

Any executable on `PATH` whose basename begins with `viti-` is exposed as a
subcommand: `viti-foo` on `PATH` becomes `viti foo [args...]`. Run
`viti plugin list` to see what is available and whether anything is shadowed
by a built-in command. Plugins inherit `VITI_AVAILABILITYZONE` and
`VITI_CONFIG` in their environment so they can read viti's global state
without reparsing flags.

### Installing plugins

Plugins listed in the curated index ([`plugins.yaml`](plugins.yaml)) install
with SHA-256 and, when `cosign` is on `PATH`, Sigstore signature verification:

```
viti plugin list --available     # what the index offers
viti plugin install <name>       # or <name>@v1.2.3
viti plugin upgrade <name>
viti plugin uninstall <name>
```

Binaries land next to `viti` itself unless `--prefix` says otherwise.

`viti upgrade` checks viti **and** every installed plugin in one pass, and
`viti upgrade --run` upgrades them all behind a single confirmation — the
plugins through the same verified path as `viti plugin upgrade`. Pass
`--no-plugins` for viti alone. Plugins installed by hand (no state file in
`~/.vitistack/plugins`) are outside its reach, as with `viti plugin upgrade`.

### Plugin aliases

An index entry may declare short aliases, installed as links beside the binary:

```yaml
- name: kubevirt
  repo: vitistack/vitictl-kubevirt
  aliases: [kv]        # `viti kv` reaches viti-kubevirt
```

Aliases are declared in the index rather than chosen at install time because a
clash is **silent**. viti only looks for a plugin when it does not recognise a
subcommand, so an alias colliding with a built-in never fires and never says
why — `viti kc` would keep meaning `kubernetescluster` on every machine that
installed it. Declaring them centrally means the clash is caught once, in
review, by the tests over `plugins.yaml`.

Three guards back that up:

- `viti plugin install` refuses an alias already claimed by a built-in command
  or by another installed plugin, before downloading anything. `--no-aliases`
  installs the plugin without them.
- `viti plugin list` re-checks on every run, because a viti release can claim a
  name months after an alias was installed — the one case install-time checking
  cannot catch. Aliases appear on their plugin's row, not as extra installs:

  ```
  NAME            VERSION  PATH                        STATUS
  kubevirt (kv)   v0.0.1   /Users/me/.local/bin/viti-kubevirt  ok
  ```

- `viti plugin uninstall` removes the links it created, and only those: a
  symlink is removed when it points at the plugin's binary, a plain file only
  when it is byte-identical. Somebody else's `viti-kv` is left alone.

Before adding an alias, check it against `viti --help` including each command's
own aliases. Prefer aliases that shorten a genuinely long name — a two-letter
alias for a three-letter plugin spends the shared namespace for almost nothing.

### Publishing a plugin

A plugin needs a GitHub release whose assets follow the layout the installer
expects, and an entry in `plugins.yaml`. With the canonical layout the entry is
just a name, a repo, and a description — no overrides:

| Asset                                       | Purpose                                 |
| ------------------------------------------- | --------------------------------------- |
| `viti-<name>-<tag>-<os>-<arch>.tar.gz`      | archive, binary at `<dir>/viti-<name>`  |
| `viti-<name>-<tag>-SHA256SUMS`              | aggregate checksums                     |
| `<archive>.cosign.bundle`                   | Sigstore signature                      |

Two details are easy to get wrong:

- The signing workflow must live at `.github/workflows/release.yml`, because
  the default cosign identity is
  `^https://github.com/<repo>/.github/workflows/release.yml@refs/tags/`.
- The installer reads gzipped tars only, and expects the inner binary to be
  named `viti-<name>` on **every** platform including Windows — it appends
  `.exe` itself. Ship a `.zip` alongside if you also want a hand-installable
  Windows artifact.

`plugins.yaml` documents the per-entry overrides for layouts that differ.

### Private plugin repositories

Public plugins need no setup. For a plugin hosted in a **private** repository,
authenticate first — release lookups and asset downloads then carry your token:

```
gh auth login          # or: export GH_TOKEN=...
viti plugin install <name>
```

The token is discovered in the gh CLI's own order: `GH_TOKEN`, then
`GITHUB_TOKEN`, then `gh auth token`. Nothing is stored; it is read per
invocation, and a token that GitHub rejects falls back to an unauthenticated
request so a stale one cannot break public installs.

This matters because GitHub answers unauthenticated requests for private
resources with **404, not 403** — so without a token a private repo is
indistinguishable from one that simply has no releases. `viti` says which case
it thinks you are in:

```
github API returned 404 for owner/repo — the repository may be private, or it
has no releases yet. For a private repository, authenticate first: set
GH_TOKEN (or GITHUB_TOKEN), or run 'gh auth login'
```

Authenticated installs fetch assets through the GitHub releases API rather than
the plain browser download URL, since private release assets are not reachable
from the latter. Unauthenticated installs keep using the plain URL, which
avoids the API's stricter anonymous rate limit.

### Extra plugin indexes

The index in this repository is public. Plugins that should not be advertised
there can live in an index of your own, listed in `~/.vitistack/ctl.config.yaml`:

```yaml
pluginindexes:
  - https://raw.githubusercontent.com/<org>/<repo>/main/plugins.yaml
```

viti reads the public index first, then each configured one, and merges them.
`viti plugin list --available` shows everything, and installs work the same
either way. On a name clash the configured index wins, so a team can override
a public entry with its own build.

`VITICTL_PLUGINS_INDEX` still replaces the default public index — handy for
testing an index in isolation. Configured indexes are merged on top of
whichever default is in effect.

A source that cannot be read is reported and skipped rather than failing the
command, so one unreachable internal index does not block installing anything
else. An error is returned only when no index could be read at all.

### Hiding an entry from listings

An entry in a shared index can be marked `private: true`:

```yaml
  - name: example
    repo: acme/viti-example
    description: Team-only helper
    private: true
```

It is then left out of `viti plugin list --available`, which reports how many
entries it withheld, and appears with `viti plugin list --all`:

```
$ viti plugin list --available
NAME  REPO               INSTALLED  DESCRIPTION
gui   vitistack/vitictl  v0.0.22    Terminal UI for viti

1 plugin(s) not shown (marked private). Use --all to include them.
```

The flag controls **advertising, not access**: the entry is still readable in
the index file and still installs normally for anyone who can read its
repository. It is for entries most readers of a shared index could not install
anyway — combine it with the token discovery above.

## Make targets

`make help` prints the full list. Key targets:

- `make build` — build `bin/viti`
- `make build-gui` — build `bin/viti-gui` (termui TUI plugin)
- `make build-all` — build both binaries
- `make install` / `make install-gui` — `go install` to `$GOBIN`
- `make test`, `make lint`, `make lint-fix`
- `make gosec`, `make govulncheck` — security scans (install tools into `./bin/`)
- `make sbom` — generate CycloneDX + SPDX SBOMs into `./sbom/`
- `make deps`, `make update-deps`
