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

### Machines (alias: `m`) — dashboard

```
# Per-node Talos dashboard. The owning cluster is inferred from the
# machine name (<clusterId>-ctp<N> / <clusterId>-wrk<N>).
viti machine console <name> [--az zone] [-n namespace]
```

### Other CRDs

The remaining Vitistack CRDs share the same `list` / `get <name>` / `search
[query]` pattern with `-o` support. Each has short aliases:

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
