# vitictl

Command-line tool for Vitistack. The binary is installed as `viti`. A
Vitistack deployment can span several Kubernetes clusters, modelled as
**availability zones**: every command aggregates across all configured zones,
with `-z/--availabilityzone` (or `--az`) to narrow to one.

## Install

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

## Make targets

`make help` prints the full list. Key targets:

- `make build` — build `bin/viti`
- `make install` — `go install` to `$GOBIN`
- `make test`, `make lint`, `make lint-fix`
- `make gosec`, `make govulncheck` — security scans (install tools into `./bin/`)
- `make sbom` — generate CycloneDX + SPDX SBOMs into `./sbom/`
- `make deps`, `make update-deps`
