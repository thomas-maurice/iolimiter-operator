# iolimiter-operator

[![CI](https://github.com/thomas-maurice/iolimiter-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/thomas-maurice/iolimiter-operator/actions/workflows/ci.yml)

A Kubernetes operator that caps a pod's disk I/O (cgroup v2 `io.max`:
bandwidth + IOPS, read and write independently) per PVC volume, declared as
a namespaced CRD instead of annotations.

## Why

Noisy-neighbour I/O on shared block devices (a backup job saturating the
disk a Postgres WAL lives on, a batch job starving a latency-sensitive
service) is otherwise only fixable node-wide or not at all. `io.max` on the
pod cgroup gives a per-pod, per-device cap that survives container and
agent restarts, with no need for `privileged: true`.

## How it works

1. You declare an `IOLimiter`: a pod selector + a list of pod volume names
   (`pod.spec.volumes[].name`, not a mount path — this is what lets one CR
   target every replica of a StatefulSet regardless of the replica's own
   PVC name) + limits.
2. A controller (Deployment, leader-elected) watches `IOLimiter`s, Pods and
   PVCs, resolves each matched pod's volumes to whole-disk devices' worth
   of desired limits, and writes one internal `PodIOLimit` object per
   limited pod (least-privilege boundary: this is the only thing the node
   agent can read).
3. A node agent (DaemonSet, **not** privileged) watches only the
   `PodIOLimit`s for its own node, resolves each volume to a `MAJ:MIN`
   whole-disk device via the host's `/proc/1/mountinfo`, and writes
   `io.max` on the **pod-level** cgroup — never per-container, so a crash
   loop or sidecar restart never leaves an unthrottled window.

```
IOLimiter (you)  ──watch──▶  controller  ──creates──▶  PodIOLimit (internal)  ──watch(nodeName)──▶  agent (DaemonSet)
                                                                                                          │
                                                                                        writes cgroup v2 io.max on the pod cgroup
```

Full design rationale, kernel facts and the algorithm are in `SPEC.md`.

## Install

Minimum Kubernetes: **1.32** (CRD selectable fields, used for the agent's
per-node `PodIOLimit` watch).

### kustomize

```bash
kubectl apply -k config/crd                # CRDs first (own lifecycle)
kubectl apply -k config/default            # namespace, RBAC, controller, agent
```

`config/default`'s namespace carries the PSA `privileged` labels the agent
needs for its hostPath mounts (see [Trust model](#trust-model)); if you
apply it to a different namespace, label that namespace yourself:

```bash
kubectl label namespace <ns> \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/audit=privileged \
  pod-security.kubernetes.io/warn=privileged
```

`config/agent/agent.yaml`'s agent cgroup hostPath defaults to `kubepods.slice`
(the common systemd layout) — check it matches your node before applying
(`kubectl describe pod` on a stuck agent pod, reason `ContainerCreating`, is
the only signal if it doesn't; nothing on the `IOLimiter`/`PodIOLimit`
status names it). On **kind**, use `hack/kind-overlay` instead of `config/default` directly —
it's the same manifests with the cgroup hostPath patched to kind's own
layout (`kubelet.slice/kubelet-kubepods.slice`):

```bash
kubectl apply -k hack/kind-overlay   # uses the published image; for a local
                                      # dev build see `make deploy-kind`
```

### Helm

```bash
helm install iolimiter-operator dist/chart \
  --namespace iolimiter-operator-system --create-namespace
```

The chart (regenerated from kustomize by `make helm-chart`, see
`SPEC.md` §8) installs both CRDs, the controller Deployment and the agent
DaemonSet. It does not label the release namespace with PSA `privileged` by
default: either label it yourself as above and use `--create-namespace`
(unchanged from before), or set `namespace.create=true` (SPEC.md D29) to let
the chart manage the namespace itself, labelled, **instead of**
`--create-namespace` (the two are mutually exclusive — Helm can't adopt a
namespace `--create-namespace` already created without the chart's own
ownership metadata):

```bash
helm install iolimiter-operator dist/chart \
  --namespace iolimiter-operator-system \
  --set namespace.create=true
```

kind clusters don't enforce PSA by default, so neither is needed there.

### Pinning the image

The shipped defaults (`config/default`'s kustomize image transform, and a
plain `helm install` with no `manager.image.tag` override, which falls back
to the chart's `Chart.appVersion`) are conveniences, not a production
recommendation (D39, security review): kustomize's own default is
`ghcr.io/thomas-maurice/iolimiter-operator:latest`, a moving tag. Pin a real
deployment to a released tag or, stronger, a digest:

```bash
# kustomize
cd config/default && kustomize edit set image \
  controller=ghcr.io/thomas-maurice/iolimiter-operator:v0.1.0
# or by digest:
cd config/default && kustomize edit set image \
  controller=ghcr.io/thomas-maurice/iolimiter-operator@sha256:<digest>

# Helm
helm install iolimiter-operator dist/chart \
  --namespace iolimiter-operator-system --create-namespace \
  --set manager.image.tag=v0.1.0
```

Every image published from `master` or a `v*` tag (`.github/workflows/ci.yml`)
is signed keylessly with `cosign` (Sigstore Fulcio/Rekor, no key to manage)
right after push, by digest. Verify before pulling:

```bash
cosign verify \
  --certificate-identity-regexp 'https://github.com/thomas-maurice/iolimiter-operator/.github/workflows/ci.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/thomas-maurice/iolimiter-operator@sha256:<digest>
```

Common values:

| Value | Default | Meaning |
|---|---|---|
| `manager.image.repository` / `manager.image.tag` | `ghcr.io/thomas-maurice/iolimiter-operator` / chart `appVersion` | Image for **both** the controller Deployment and the agent DaemonSet (one image, two modes, SPEC.md D12) |
| `manager.args` | `[--leader-elect]` | Extra flags on `/manager` (controller mode), e.g. `--zap-log-level=debug` |
| `agent.args` | `[]` | Extra flags on `/manager agent`, e.g. `--zap-log-level=debug` |
| `agent.resyncPeriod` | `60s` | `--resync-period`: drift-correction interval (D11) |
| `agent.allowRootDevice` | `false` | `--allow-root-device`: opt in to throttling any device backing `agent.protectedPaths` below, not just the root disk (D23/D34) |
| `agent.protectedPaths` | `[/, /var/lib/kubelet, /var/lib/containerd, /var/lib/containers]` | `--protected-paths`: host paths whose whole-disk device is refused unless `agent.allowRootDevice` is set — node root, kubelet's directory and the container runtime's own storage directories (their writable layers share the root disk's fsync-inversion risk, K7, D34) |
| `agent.kubepodsCgroup` | `""` (auto-discover) | `--kubepods-cgroup` override (D6) |
| `agent.cgroupHostSubtree` | `kubepods.slice` (typical systemd node) | Host path under `/sys/fs/cgroup` bind-mounted into the agent (D28); set to `kubelet.slice/kubelet-kubepods.slice` on kind (see `hack/kind-values.yaml`), or `kubepods`/`kubelet/kubepods` for cgroupfs. A wrong value fails every agent pod at `ContainerCreating` with no signal on the `IOLimiter`/`PodIOLimit` status — `kubectl describe pod` on a stuck agent pod is the only place it shows up. |
| `admissionPolicy.enabled` | `true` | Install the `ValidatingAdmissionPolicy`s restricting `podiolimits` writes (D27); disable only if your cluster doesn't support `admissionregistration.k8s.io/v1` VAPs |
| `namespace.create` | `false` | Let the chart create and PSA-label the release namespace itself (D29) — use **instead of** `--create-namespace` |
| `rbac.helpers.enabled` | `false` | Install the `iolimiter-{admin,editor,viewer}` ClusterRoles |

```bash
helm install iolimiter-operator dist/chart \
  --namespace iolimiter-operator-system --create-namespace \
  --set manager.image.tag=v0.1.0 \
  --set agent.resyncPeriod=30s
```

`--set manager.image.tag=X` puts `X` on both the Deployment and the
DaemonSet.

## Uninstall

Delete every `IOLimiter` first and wait for the `PodIOLimit`s they created
to be gone — this runs the D9 release handshake (agent resets `io.max`,
controller removes the finalizer) while the controller and agent are still
running:

```bash
kubectl delete iolimiter -A --all
# poll until empty:
kubectl get podiolimit -A
```

Only then uninstall the controller/agent themselves:

```bash
# Helm
helm uninstall iolimiter-operator -n iolimiter-operator-system

# kustomize
kubectl delete -k config/default
kubectl delete -k config/crd   # only once no PodIOLimit remains (see below)
```

If you skip the wait (or the controller was already down): any live
`PodIOLimit` keeps its finalizer, and a namespace containing one **cannot
finish deleting** — the namespace controller waits for it, same as any
stuck finalizer. `crd.keep: true` (Helm) and a plain `kubectl delete -k
config/default` (kustomize) both leave existing CRDs/CRs in place for
exactly this reason: uninstalling the operator does not un-throttle
anything on its own.

**Stuck-finalizer recovery** (controller unavailable, or you uninstalled
without deleting `IOLimiter`s first): the write VAP (D27, revised by D33)
allows a **metadata-only** update to `podiolimits` — including removing a
finalizer — from anyone RBAC permits, not only the controller service
account. This means a cluster-admin can recover by hand without touching
the VAP or its binding:

```bash
kubectl patch podiolimit <name> -n <ns> --type=merge \
  -p '{"metadata":{"finalizers":null}}'
```

Do this only once you've confirmed (or accepted) that the pod's `io.max`
rule will **not** be reset — stripping the finalizer this way skips the
agent's reset step D9 would otherwise guarantee. If the agent is actually
still running, prefer deleting the `IOLimiter`/`PodIOLimit` normally (above)
and letting the handshake complete instead of reaching for this.

## API reference

### `IOLimiter` (namespaced, user-facing, `iol`)

| Field | Type | Meaning |
|---|---|---|
| `spec.podSelector` | `metav1.LabelSelector` (required) | Pods in this namespace to limit. `{}` selects every pod. |
| `spec.volumes[].name` | string | `pod.spec.volumes[].name` — **not** a mount path (D1) |
| `spec.volumes[].limits.readBytesPerSecond` / `writeBytesPerSecond` | `resource.Quantity`, e.g. `10Mi` | Whole number of bytes, 1Ki..1Pi (enforced controller-side, see the CEL-budget deviation note in `SPEC.md` C0) |
| `spec.volumes[].limits.readIOPS` / `writeIOPS` | `int64` | 2..4294967295 |
| `status.matchedPods` / `appliedPods` / `failedPods` | `int32` | Scheduled, non-terminal pods matched by the selector with ≥1 listed volume present |
| `status.conditions[Ready]` | | `True/AllApplied`, `True/NoMatchingPods`, `False/Pending`, `False/Failed` — message names up to 5 failing pods |

At least one limit field must be set (CEL). Overlapping `IOLimiter`s on the
same pod volume merge to the **per-field minimum** (most restrictive,
order-independent, D2) — a limiter can tighten another's rule but never
loosen it. See `config/samples/storage_v1alpha1_iolimiter_baseline.yaml` +
`_strict.yaml` for a worked example.

### `PodIOLimit` (namespaced, **internal**, `pil`)

Controller-written spec, agent-written status, one per limited pod,
ownerRef → the pod. Not something you create by hand — inspect it for
debugging (`kubectl get pil -n <ns>`, `status.volumes[]` names the
resolved device and state per volume, `status.devices[]` is the agent's
ownership record). Full schema in `SPEC.md` §4.2.

## Mapping to `io.max`

Every IO the pod cgroup sends to a device is throttled — this is a kernel
property of `io.max`, not a bug: it's per (cgroup, device), not per mount.
Consequences:

- **Two volumes on the same disk share one rule**: the per-field minimum
  of every limit naming that volume, reported as `state: Applied, reason:
  SharedDevice` with a message naming the other volumes and the effective
  rule (D7).
- **`io.max` has no cross-device total.** A pod with volumes on two disks
  gets two independent rules, never a combined budget across them.
- **Rootfs/emptyDir on the same disk as a limited volume are throttled
  too** (K7) — this is why the node's protected paths (root, kubelet's
  directory, and the container runtime's own storage directory —
  `/var/lib/containerd`, `/var/lib/containers`, `--protected-paths`, D34)
  are refused by default (`Unsupported/SharedRootDevice`, opt in per node
  with `--allow-root-device`/`agent.allowRootDevice`): throttling any of
  those also throttles every pod's rootfs/writable layer on that node, and
  under ext4 `data=ordered` other cgroups' `fsync` can queue behind the
  throttled pod's writeback. This isn't limited to the root disk: **any**
  volume that shares a filesystem with a throttled pod's rootfs or another
  pod's volume (e.g. several `local` PVs as directories on one shared ext4
  data disk) has the same inversion risk — one filesystem per volume
  (block-based CSI, not directory-based `local` PVs sharing a disk) avoids
  it.
- **Partitions aren't throttleable** (`io.max` rejects them): the agent
  always resolves to the whole disk. This also means **stacked block
  devices** (e.g. an LVM volume on `dm-0` backed by `sda2`) get two
  independent rules that both apply to the same IO — a bio submitted to
  the dm device is cloned down to the backing disk with the same cgroup
  association, so a rule on the dm device and a rule on its backing disk
  both throttle it (IOPS on every kernel; BPS double-charging is avoided
  on kernels with `BIO_BPS_THROTTLED`, 5.x+). Size limits on LVM/md-backed
  nodes with this in mind.

### Dirty pages and OOM

`io.max` throttles the writeback of already-dirtied pages, not the
`write()` syscall itself for buffered I/O: an app can still dirty pages in
the page cache faster than the throttled device drains them. If a write
burst outruns `writeback` before the kernel's `dirty_ratio` backpressure
kicks in, the container's memory cgroup can hit its `limits.memory` and get
OOMKilled — with `io.max` looking, misleadingly, like the cause. See
`.ideas/dirty-pages-and-oom.md` for the full mechanism and mitigations
(`memory.high` headroom, `O_DIRECT`, lowering `vm.dirty_ratio`). Managing
`memory.high` automatically is out of scope for this operator (§13).

## Supported volume types (D19)

| Source | Supported | `kubeletDirName` |
|---|---|---|
| `persistentVolumeClaim` (Bound, `volumeMode: Filesystem`), backed by a `local` or any other PV kubelet mounts under the pod's kubelet volumes directory | yes | PV name |
| `persistentVolumeClaim` backed by a **`hostPath`-source** PV (e.g. k3s/Rancher `local-path-provisioner`) | **no** | — kubelet never mounts it under `pods/<uid>/volumes/`, so the agent could never resolve a device for it; reported `UnsupportedVolumeType` on the `IOLimiter`, naming the PV |
| generic `ephemeral` (claim `<pod>-<vol>`) | yes | PV name |
| inline `csi` | yes | volume name |
| `hostPath` (inline pod volume), `emptyDir`, `configMap`/`secret`/`projected`, block-mode PVC | **no** | — reported `UnsupportedVolumeType` on the `IOLimiter`, not put in the `PodIOLimit` |

An unbound PVC leaves the pod `Pending` for that volume until the PVC
watch fires; it's not an error.

## Trust model (§7)

`IOLimiter` is **admin-imposed**: any namespace user who can create one can
throttle any pod (matching their own selector) in their own namespace —
this is the intended blast radius, same as any other namespaced CRD. The
shipped `iolimiter-{admin,editor,viewer}` ClusterRoles (installed by
default via kustomize's `config/rbac`, gated behind
`rbac.helpers.enabled: false` by default in the Helm chart) are
**deliberately not** aggregated into the built-in `edit`/`admin` roles
(no `rbac.authorization.k8s.io/aggregate-to-{edit,admin}` label): granting
I/O-throttle power is an explicit, separate decision from ordinary
namespace edit access.

Granting `iolimiters` `create` to a tenant is bounded (D37): `podSelector`
is size-capped (≤16 `matchExpressions`, ≤64 values each, ≤16
`matchLabels`), and the controller parses each limiter's selector once per
generation, not per pod event, so a malicious selector can't degrade the
shared controller for everyone. If you grant `iolimiters` create to
untrusted tenants, also apply a `ResourceQuota` bounding how many they can
create, e.g.:

```yaml
apiVersion: v1
kind: ResourceQuota
metadata:
  name: iolimiter-quota
spec:
  hard:
    count/iolimiters.storage.maurice.fr: "10"
```

**`IOLimiter` is a ceiling on what it selects, not an enforcement boundary
against the namespace's own editors** (D38): it throttles the pods/volumes
it currently matches and resolves, nothing more. A namespace editor who can
rename a pod's volume or relabel/recreate the pod can make it stop matching
and escape the limit — this isn't a bug to fix, it's the same trust
boundary every namespaced selector-based Kubernetes object has (`NetworkPolicy`,
`PodDisruptionBudget`, ...): whoever can edit the selected objects can
already opt out of anything selector-based. The `IOLimiter` status reports
`selectedPodsWithoutVolumes` (pods matching `podSelector` that carry none
of the listed volumes) and a `SelectedPodWithoutVolume` Warning event, so
an admin *sees* the escape instead of a silent, indistinguishable
`NoMatchingPods`. When an `IOLimiter` is meant to bind every pod in a
namespace regardless of relabelling, use `podSelector: {}` (matches all
pods) rather than a label selector a tenant could remove.

Multi-tenant isolation additionally requires **one filesystem per PV**: two
pods (different tenants) with PVs on the same shared `ext4`/`xfs`
filesystem can defeat each other's throttle via the kernel's own fsync
serialization (K7, "fsync inversion" — an unthrottled writer's `fsync` on
the shared filesystem journal can stall a throttled tenant's I/O
regardless of their own `io.max` limits). `iolimiter-operator` does not, and
cannot, detect this from userspace; it's a PV/StorageClass provisioning
decision (one block device or filesystem per PV) outside this project's
scope.

There is **no** user-facing role for `PodIOLimit` — it's internal (see the
`kubectl explain podiolimit` output and the type's doc comment: created and
managed by the `IOLimiter` controller, one per limited pod; don't create or
edit it by hand, use `IOLimiter`), and anyone who could create or edit one
directly could throttle any pod on any node by UID (the agent trusts
`spec.nodeName`/`spec.podUID` as given). RBAC alone can't express "only the
controller", so C7 adds two `ValidatingAdmissionPolicy`s
(`admissionPolicy.enabled: true` by default, SPEC.md D27), installed by
both `config/vap` (kustomize) and the Helm chart: `podiolimits` (the main
resource) create/update is denied for everyone but the controller
ServiceAccount, and `podiolimits/status` update is denied unless the
caller is the agent ServiceAccount **and** its own bound token's
`node-name` claim matches the object's `spec.nodeName` (this is why the
minimum Kubernetes version is 1.32). A **metadata-only** update (no spec
change, `object.spec == oldObject.spec`) is allowed from anyone RBAC
permits — the carve-out that lets a cluster-admin strip a stuck finalizer
during uninstall without a Helm pre-delete hook (D33, see
[Uninstall](#uninstall)); a spec-changing update stays controller-SA-only.
**`delete` is deliberately not VAP-restricted** (revised after C7): RBAC
already gates who may delete a
`PodIOLimit` at all (nobody but the controller's own ClusterRole gets
`delete` on `podiolimits`), and the D9 finalizer still makes the agent
reset the kernel rule before the object is actually removed no matter who
issued the delete, so restricting DELETE bought nothing RBAC + the
finalizer don't already cover — while it did block a cluster-admin's own
manual cleanup and, more importantly, Kubernetes' own garbage collector
and namespace controller, which could otherwise leave a namespace stuck
`Terminating` for as long as the controller was down. RBAC (below) is
still the first line of defense — don't grant `podiolimits`
`create`/`update` outside the controller's own ClusterRole — the VAP is
defense in depth for the case where RBAC is accidentally over-granted.

The agent itself is least-privilege: it can only `get/list/watch
podiolimits` and `get/update/patch podiolimits/status`, cluster-wide (RBAC
can't express a per-node field selector, so it can *read* every
`PodIOLimit`'s pod name/UID/limits, but never write a spec or read Pods,
PVCs or Secrets); its host cgroup mount is narrowed to the kubepods
subtree only (`agent.cgroupHostSubtree`, SPEC.md D28) — it can no longer
reach `system.slice`/`kubelet.service`, so it can't cause a node-wide DoS
via `memory.max`/`cgroup.freeze`/`cgroup.procs` on unrelated cgroups; and
its `/proc` mount is a single file (host `/proc/1/mountinfo`, `type:
File`), not a directory bind-mount of the whole host `/proc` — a `type:
Directory` mount would have let a uid-0 process in the agent container
jump through the magic symlink `/proc/1/root` into the **host's own
rootfs** (gated only by a same-uid ptrace check — no capability, no
`privileged`, no `hostPID` needed) and read or write anything uid 0 owns
on the node (`/etc/shadow`, kubelet's client certificate, every Secret
volume mounted on that node). With the mount narrowed to one file, a
compromised or buggy agent's blast radius is exactly the kubepods cgroup
subtree on its own node: it can write any `io.max` file under it (throttle
or freeze any pod scheduled to that node), and read/forge any
`PodIOLimit`'s status cluster-wide — but it cannot reach the host
filesystem, `system.slice`, or any other node's cgroups. See `SPEC.md` §7
for the full RBAC table.

**The operator namespace (`iolimiter-operator-system` by default) is
cluster-admin-equivalent** (D39): whoever can create a Pod there can mount
the controller's and agent's ServiceAccount tokens (`automountServiceAccountToken`
isn't disabled on either) and, for the agent specifically, request the
same privileged PSA level and hostPath/cgroup mounts the DaemonSet itself
uses — from there, the agent's own blast radius above (§ above) applies
directly. Treat `create`/`patch pods` in the operator namespace as
equivalent to a cluster-admin grant; don't hand it out casually, and don't
colocate unrelated workloads in that namespace.

An `IOLimiter` in `InvalidLimit`/`InvalidSelector` state (a limit that
fails validation, or a selector rejected by D37's bounds) never loosens or
removes an already-applied limit (D36): the affected `PodIOLimit`s keep
that specific limiter's **last good** contribution until the `IOLimiter` is
fixed. A broken edit degrades to "no effect from this edit", not "no
throttle at all".

## Migrating from the annotation mode (D3)

The pre-rewrite `internal/limiter` DaemonSet (`blkio-limiter.maurice.fr/*`
pod annotations, 5s polling, `privileged: true`) was removed in C8. No
shim was shipped — translate annotations to an `IOLimiter` by hand:

| Legacy annotation | `IOLimiter` equivalent |
|---|---|
| `blkio-limiter.maurice.fr/<name>: "path=<mount> riops=<n> wiops=<n> rbps=<n> wbps=<n>"` | `spec.volumes[]` entry, `name: <pod volume whose mount was <mount>>`, `limits: {readIOPS: <n>, writeIOPS: <n>, readBytesPerSecond: <n>, writeBytesPerSecond: <n>}` |
| Annotation key suffix (`<name>`) | Not carried over — `IOLimiter` groups all volumes for a pod selector into one CR; pick any CR name |
| `path=<mount>` (container mount path) | **Not used.** Identify the volume by `pod.spec.volumes[].name` instead (D1) — check your pod/sts spec, not the annotation |
| Per-pod annotation, matched by presence | `spec.podSelector` (label-based) — annotate-per-pod has no direct equivalent; add a label if you don't have one |
| `rbps`/`wbps` as raw byte integers | `resource.Quantity`, e.g. `5242880` → `5Mi` |
| Reset by removing the annotation | Reset by deleting the `IOLimiter` (or removing the pod from its selector) |
| hostPath volumes (the legacy test fixture) | **Unsupported** (D19/OQ4) — hostPath isn't kubelet-mounted under a discoverable path (K8) |

## Logging & debugging

Both the controller and the agent run a controller-runtime manager and log
through `logr`/zap (`--zap-log-level`, `--zap-encoder`, `--zap-devel`).
Default: console encoder, **info** level.

Key vocabulary (literal strings at call sites, not shared constants — the
`logcheck` linter requires inlined keys):

| Key | Meaning |
|---|---|
| `pod` | `<ns>/<name>` of the target pod (every agent and PodReconciler line) |
| `podUID` | pod UID |
| `iolimiter` | `<ns>/<name>` of an IOLimiter (every IOLimiterReconciler line) |
| `podiolimit` | `<ns>/<name>` of the PodIOLimit |
| `node` | node name |
| `volume` | pod volume name |
| `kubeletDir` | `kubeletDirName` (PV name or volume name) |
| `mountPoint` | host mount point matched in mountinfo |
| `mountDevice` | `MAJ:MIN` from mountinfo |
| `device` | whole-disk `MAJ:MIN` actually throttled |
| `partitionOf` | set when `mountDevice` was a partition |
| `cgroup` | pod cgroup path relative to the cgroup root |
| `path` | full `io.max` path written |
| `before` / `after` | the device's `io.max` line before the write / on read-back (`<none>` = unlimited) |
| `rule` | the four-key rule written |
| `op` | `apply` / `reset` / `release` |
| `state` / `from` / `to` | volume/device state, or a transition's endpoints |
| `reason` | why (§4.2 reason vocabulary, or a skip reason) |
| `generation` / `observedGeneration` | spec generation handled |
| `finalizer` | finalizer name on add/remove |
| `elapsed` | time since first seen `Pending` |

Levels: **Info** = every state change and mutating API/kernel write, never
unchanged state on a resync. **V(1)** (`--zap-log-level=debug`) = waits,
requeues, no-ops, status-write conflicts. Errors are returned, not
logged-and-returned.

Raising verbosity on a running deployment:

```bash
# controller
kubectl -n iolimiter-operator-system patch deployment iolimiter-operator-controller-manager \
  --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--zap-log-level=debug"}]'

# agent (same JSON patch shape)
kubectl -n iolimiter-operator-system patch daemonset iolimiter-operator-agent \
  --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--zap-log-level=debug"}]'
```

Or via Helm: `--set 'manager.args={--leader-elect,--zap-log-level=debug}'` /
`--set 'agent.args={--zap-log-level=debug}'`.

Following one object end to end:

```bash
kubectl -n iolimiter-operator-system logs deploy/iolimiter-operator-controller-manager | grep '<ns>/<iolimiter-name>'
kubectl -n iolimiter-operator-system logs ds/iolimiter-operator-agent --prefix | grep '<ns>/<pod-name>'
```

Use `--prefix` (or `-l control-plane=agent --prefix`) on the DaemonSet:
`kubectl logs ds/<name>` silently picks only one of the agent pods.

## Local development

See `SPEC.md` (living design doc) and `.claude/skills/working-here/SKILL.md`
(day-to-day workflow: kind cluster, `make test`/`make lint`/`make e2e`, the
API-change checklist).

```bash
make kind-setup    # pinned kind cluster + loop devices + storage class
make deploy-kind    # build, load, deploy (kustomize)
make e2e-setup && make e2e   # full e2e suite
```

## Requirements

- Kubernetes 1.32+
- cgroups v2 on every node (default on all modern distros and managed k8s)
- A block device backing the volumes you want to limit (K5: overlay,
  tmpfs, NFS, CephFS, FUSE, btrfs subvolumes and ZFS have no request queue
  and can't be throttled — reported `Unsupported/NoBlockDevice`)
- **SELinux-enforcing nodes** (e.g. CRI-O/OpenShift) need
  `seLinuxOptions.type: spc_t` on the agent's pod/container
  securityContext (`agent.podSecurityContext`/`agent.securityContext` in
  the Helm chart, or patch `config/agent/agent.yaml`) — the default
  `container_t` policy denies writes to `cgroup_t` and the host cgroup
  filesystem. **Untested** by this project's own e2e (kind's node isn't
  SELinux-enforcing); if the agent's `io.max` writes or resets start
  failing with `EACCES` after this, add `spc_t` first.

## Credits

Built with the help of [Claude Code](https://claude.ai/claude-code).
