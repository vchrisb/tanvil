# tanvil

Manage a [Talos Linux](https://www.talos.dev) cluster described by a single
`tanvil.yaml`. The tool talks to the cluster through the Talos and
Kubernetes Go client libraries: neither `talosctl` nor `kubectl` is needed.

The name joins **T**alos and **anvil**: in Greek myth the bronze giant Talos
was forged by Hephaestus to guard Crete.

Supports Talos **v1.14.0 and newer** (built against Talos v1.14.7).

## Build

```shell
CGO_ENABLED=0 go build -ldflags "-s -w -X github.com/vchrisb/tanvil/internal/cli.Version=$(git describe --tags --always)" -o tanvil .
sudo install tanvil /usr/local/bin/tanvil
```

The result is a single static binary; cross-compile by adding e.g.
`GOOS=linux GOARCH=amd64`.

## Cluster definition

`tanvil.yaml` is a Kubernetes-style object. Unknown fields are rejected.
See [examples/tanvil.yaml](examples/tanvil.yaml).

```yaml
apiVersion: tanvil.dev/v1alpha1
kind: TalosCluster
metadata:
  name: homelab
spec:
  endpoint: k8s.example.com        # Kubernetes API: https://k8s.example.com:6443
  secretsFile: secrets.yaml        # relative paths resolve against this file
  versions:
    talos: v1.14.7                 # installer tag and machine config contract
    kubernetes: 1.35.0
  image:
    platform: metal                # factory.talos.dev/<platform>-installer/<id>:<talos>
    schematic:                     # posted to factory.talos.dev to get <id>
      customization:
        systemExtensions:
          officialExtensions: [siderolabs/iscsi-tools]
  install:
    diskSelector: disk.dev_path == "/dev/sda"   # CEL expression
    wipe: false
  configPatches:                   # a file path or an inline patch document
    all: [patches/common.yaml]
    controlPlane: []
    worker:
      - apiVersion: v1alpha1
        kind: KubeNodeConfig
        labels: {node-role.kubernetes.io/worker: ""}
  network:
    interface: ens3
    prefix: 24
    gateway: 10.0.0.1
  nodes:
    - name: cp-01
      role: controlplane           # controlplane | worker
      address: 10.0.0.11
    - name: worker-01
      role: worker
      address: 10.0.0.21
      patches:                     # this node only
        - apiVersion: v1alpha1
          kind: UnattendedInstallConfig
          provisioning:
            diskSelector:
              match: disk.dev_path == "/dev/nvme0n1"
```

Every node first gets three generated documents: `UnattendedInstallConfig`
(installer image and `spec.install`), `HostnameConfig` (the node name) and
`LinkConfig` (static address and default route from `spec.network`). Then the
patches are applied in order, each overriding the ones before:
`configPatches.all`, `configPatches.controlPlane` or `configPatches.worker`, and
the node's own `patches`. A node patch can therefore override a generated
document, such as the install disk above.

A patch is either a path to a patch file (relative to `tanvil.yaml`) or
an inline strategic merge patch, exactly like `talosctl --config-patch`. Write
patches in the Talos v1.14 document style (`KubeNodeConfig`, `KubeletConfig`,
...); `genconfig` and `update` reject a setting declared both the old and the new way.
JSON6902 patches cannot be used, because Talos v1.14 configs are
multi-document.

Run `tanvil validate` to check the file without contacting anything.

## Commands

| Command | What it does |
| --- | --- |
| `gensecrets` | Create the secrets bundle at `spec.secretsFile`. |
| `genconfig [node]` | Validate and write `<node>.yaml` machine configs for review or export. |
| `update [node]` | Render the configs, show each node's diff, and apply the changed ones (`--maintenance IP` for a fresh node). |
| `bootstrap` | Bootstrap etcd on the first control plane node and write `./kubeconfig` (checked for `--force` before bootstrapping). |
| `upgrade [node]` | Upgrade Talos on all nodes one at a time (control planes first) or on one node. |
| `upgrade-k8s` | Upgrade Kubernetes to `spec.versions.kubernetes`, then run the health check. |
| `diff [node]` | Show where the running cluster differs from the definition; exits 1 on drift. |
| `reset [node]` | Reset nodes to their initial state, one at a time, workers first (destructive, confirmed per node). |
| `health` | Run the Talos cluster health check against the configured nodes. |
| `kubeconfig` | Write the admin kubeconfig to `./kubeconfig`. |
| `talosconfig` | Write a `talosconfig` for use with `talosctl`. |
| `validate` | Validate `tanvil.yaml`. |

Global flags: `-c/--config` (default `tanvil.yaml`), `--dry-run`,
`--redact` (default true, see below),
`--force` (overwrite existing output files; for `upgrade`, also upgrade nodes
that are already up to date).

`update` renders each node's config from `tanvil.yaml` itself, exactly
like `genconfig`, so the `<node>.yaml` files are never needed and can never be
stale; use `genconfig` only to inspect or export the configs. Rendering runs the
same static checks as `talosctl validate --mode metal` (schema errors, a
missing install disk, a setting declared both in the legacy v1alpha1 fields and
in its Talos 1.14 document) for every selected node first: if any config is
invalid, `genconfig` writes nothing and `update` touches no node. Checks that
need the node itself, such as whether the install disk exists, only happen when
the config is applied.

Before `update` changes anything, every node also passes the
[node pre-flight check](#node-safety-checks) and compares its running config
with the new one in an `apply-config` dry run, which runs the node's own
validation too. Then, node by node:

```console
==> cp-01 (10.0.0.11): no changes
==> worker-01 (10.0.0.21):
    --- a
    +++ b
    ...
Apply this change to worker-01? (y/n):
```

Nodes without changes are skipped entirely, so re-running `update` on an
unchanged cluster is quick and touches nothing. Changed nodes are applied after
confirmation (`-y/--assume-yes` skips the question), followed by the pod check.
`update --dry-run` only prints the diffs and exits with status 2 when any node
would change. `--maintenance` applies directly to a fresh node, since there is
no running config to compare with.

Output never shows the cluster secrets: private keys (also as PEM text), the
bootstrap and machine tokens, and the encryption and cluster secrets are
replaced by `*** redacted ***`, so `diff` and `update` output is safe to paste
or keep in CI logs. Certificates stay visible. `--redact=false` shows
everything, for debugging.

`tanvil` authenticates to the Talos API with a client certificate it mints
in memory from the secrets bundle, against all control plane addresses. The
`talosconfig` and `kubeconfig` files are only for your own use with `talosctl`
and `kubectl`.

### New cluster

```shell
tanvil gensecrets
tanvil update cp-01 --maintenance 10.0.0.99 --skip-pod-check   # DHCP address in maintenance mode
# ... repeat for every node ...
tanvil bootstrap
tanvil health
```

### Rebuilding a node

```shell
tanvil reset worker-01 --system-labels-to-wipe STATE,EPHEMERAL   # keep Talos installed
tanvil update worker-01 --maintenance 10.0.0.99 --skip-pod-check  # its DHCP address
```

`reset` uses the same reset API as `talosctl reset` and asks before every node
(`-y/--assume-yes` skips that). By default it wipes the whole system disk
(`--wipe-mode all`) and reboots; the node then needs boot media or PXE to reach
maintenance mode. Wiping only the `STATE` and `EPHEMERAL` partitions keeps the
installation, so the node boots into maintenance mode from disk. Unlike
`talosctl reset`, it reboots by default; `--reboot=false` shuts the node down.

Nodes are reset one at a time, workers first. A graceful reset (the default)
drains the node and leaves etcd before wiping it, and the command waits until the
node has stopped its services. The last control plane node of the cluster cannot
leave etcd, so it is always reset without `--graceful`. Nodes already in
maintenance mode are skipped. After a reset a node no longer has its static
address, so `reset` does not wait for it to come back.

## Node safety checks

Before `update` and `upgrade` change anything, every selected node is asked for
its machine status: it must be in the `running` or `booting` stage and report
all readiness conditions as met (Talos's own view, e.g. network, time sync,
services). A node that is unreachable, in maintenance or mid-upgrade, or has
unmet conditions stops the command before any node is touched, and all problems
are listed together. `--allow-not-ready` ignores unmet conditions, for example
to repair a node that is stuck.

Before `update` and `upgrade` change a node, all pods that have not succeeded
must be `Running` with every container ready. The kubeconfig for this check is
fetched over the Talos API from this cluster, so it can never hit another
cluster's context. After the change, the pods are checked every 15 seconds for
up to 300 seconds, and two consecutive healthy snapshots are required. When a
check fails you can retry, cancel, or explicitly continue.

`upgrade --dry-run` still performs the initial pod check but skips the
post-change wait, and only prints the node and target image.
`update --dry-run` changes no node, so it runs no pod check.

Use `--skip-pod-check` to bypass both checks, for example while building a
cluster or when the Kubernetes API is intentionally unavailable.

## Drift

`diff` compares the running cluster with the definition and changes nothing:

```console
$ tanvil diff
==> cp-01 (10.0.0.11): in sync
==> worker-01 (10.0.0.21):
    talos: running v1.14.6, want v1.14.7
    config:
      --- a
      +++ b
      ...
==> kubernetes: in sync (1.35.0)
```

Per node it reports the Talos version and Image Factory schematic the node
actually booted (not the image its machine config names, which can be ahead of
an unrebooted node) and the difference between the running machine config and a
config freshly rendered from `tanvil.yaml` (an `apply-config` dry run on
the node). The Kubernetes version is detected the way `upgrade-k8s` does. The
exit status is 1 when anything differs.

## Upgrades

`upgrade` uses the Talos v1.14 lifecycle API for each node: pull the installer
image, install it, cordon and drain the Kubernetes node (`--no-drain` to skip),
reboot and wait for the node to come back with a new boot ID, wait for the
Kubernetes node to be `Ready`, uncordon, and run the pod check.

The drain evicts pods, so it honors PodDisruptionBudgets, and gives up after
`--drain-timeout` (default 5m). A failed drain stops the upgrade before the
reboot and uncordons the node again; the new version is already installed
and takes effect at the node's next reboot. With `--delete-if-eviction-fails`, a
failed drain is retried by deleting the remaining pods instead (like
`kubectl drain --disable-eviction`), which bypasses PodDisruptionBudgets; the
retry gets another `--drain-timeout`.

Nodes that already run `spec.versions.talos` with the configured schematic are
skipped, so an interrupted rolling upgrade can simply be started again;
`--force` upgrades them anyway.

Between nodes it asks before continuing. `-y/--assume-yes` never asks;
`--auto-continue-on-ready` only skips the question when the pod check passed.
After a multi-node upgrade the cluster health check runs.

## Development

```shell
go test ./...
go vet ./...
```

### Upgrading the Talos dependency

The operations in `internal/cluster` reimplement `talosctl` commands with the Go
bindings. Each one carries a `TALOS-SYNC` comment naming the `talosctl` source
file it mirrors. When bumping `github.com/siderolabs/talos`, list them with

```shell
grep -rn TALOS-SYNC internal/
```

then diff each named file between the old and new Talos tag and port any
behaviour changes, so tanvil keeps doing what `talosctl` does.

## License

[Mozilla Public License 2.0](LICENSE), the same license as Talos.
