# k8s-blkio-limiter — SPEC

Living spec + progress tracker. **Update the checkboxes as chunks land.** If a
session dies, resume from the first unchecked item of the first unfinished chunk.
Generated 2026-09-23 by the architect. Conventions mirror
`../pvc-downsize-operator` (same author, same API group).

- Module: `github.com/thomas-maurice/k8s-blkio-limiter` (unchanged)
- API: `storage.maurice.fr/v1alpha1`
  - `IOLimiter` — namespaced, user-facing, short name `iol`
  - `PodIOLimit` — namespaced, **internal** (controller-written spec, agent-written status), short name `pil`
- Image: `ghcr.io/thomas-maurice/k8s-blkio-limiter` — one image, two modes: `/manager` (controller) and `/manager agent` (node agent)
- Scaffold: kubebuilder v4 (`go/v4`) + `helm/v2-alpha` plugin (chart in `dist/chart`)
- Install namespace: `k8s-blkio-limiter-system` (PSA `privileged`, see D14)
- Minimum Kubernetes: **1.32** (CRD selectable fields GA; see OQ3)

## 1. Goal

Replace the annotation-driven, 5s-polling, `privileged: true` DaemonSet with a
CRD-driven operator: users declare `IOLimiter`s (pod selector + pod volume
names + limits); a central controller compiles them into one `PodIOLimit` per
limited pod; a least-privilege node agent enforces each `PodIOLimit` by
writing cgroup v2 `io.max` on the **pod-level** cgroup for the whole-disk
device backing each volume, and reports back through `PodIOLimit.status`.
Done = the kind e2e proves throttling by measured throughput, rules survive
container and agent restarts, and removing a limit resets exactly the rules we
wrote and nothing else.

## 2. Kernel & node facts the design relies on

| # | Fact | Consequence |
|---|------|-------------|
| K1 | `io.max` parses **one** `MAJ:MIN key=val…` rule per `write()`. A second line fails with `EINVAL` (its `MAJ:MIN` token has no `=`) and the whole write is dropped. | One `write()` per device. **Legacy bug:** `internal/limiter/apply.go` joins every rule into one write, so a pod with two volumes on two devices gets no limit at all. |
| K2 | A partial rule keeps the other keys at their **previous** values (`tg_set_limit` starts from the current config). | Always write all four keys, `max` for unset. |
| K3 | Writing `rbps=max wbps=max riops=max wiops=max` removes the device line. | This is "reset". Reading back, a missing line means unlimited. |
| K4 | `io.max` rejects partitions (`bdev_is_partition` → `ENODEV`). | Resolve partition → whole disk: `/sys/dev/block/M:m/partition` exists ⇒ `EvalSymlinks(/sys/dev/block/M:m)`, `Dir`, read `dev`. **Do not** use lexical `filepath.Join(.., "..")`: it resolves `..` before the symlink and gives the wrong answer. |
| K5 | Major 0 (overlay, tmpfs, NFS, CephFS, FUSE, **btrfs subvolumes, ZFS**) has no request queue and cannot be throttled. | Volume `Unsupported`, reason `NoBlockDevice`. |
| K6 | Values ≤ 1 are ignored by `tg_set_limit` (`val > 1` guard, from memory: **verify in C1 on kind**). IOPS are capped at `UINT_MAX`. | CEL: IOPS in [2, 4294967295]. The agent reads back after every write (D11), so a silently ignored value shows up as `Failed/KernelMismatch`, never as a false "Applied". |
| K7 | `io.max` is per (cgroup, device), **not** per mount. Every IO the cgroup sends to that device is throttled, including rootfs/emptyDir if they are on the same disk. blk-throttle is hierarchical: a pod-level limit caps every child container, and writeback is charged to the dirtying memcg (a child of the pod). | Pod-level cgroup (D5). Two volumes on the same device → one rule, min per field (D7). OQ1 covers the shared-root-disk hazard. |
| K8 | Kubelet mounts every PVC/CSI volume on the host at `<kubeletRoot>/pods/<podUID>/volumes/<plugin>/<dirName>[/mount]` **before** containers start (`dirName` = PV name for PVC/generic-ephemeral, pod volume name for inline CSI). hostPath volumes are **not** mounted there (the runtime bind-mounts them straight into the container). | Device resolution = parse host `/proc/1/mountinfo`, match that mount point. No PIDs, no container IDs, no tree walk (D6). hostPath/emptyDir unsupported (OQ4). |
| K9 | Pod cgroup paths are deterministic from (kubepods root, driver, QoS, UID). systemd: `<P>.slice/<P>-<qos>.slice/<P>-<qos>-pod<uid with _>.slice`, Guaranteed `<P>.slice/<P>-pod<uid_>.slice`. cgroupfs: `kubepods/<qos>/pod<uid>`, Guaranteed `kubepods/pod<uid>`. `<P>` is `kubepods` normally, and `kubelet-kubepods` under `kubelet.slice/` on kind (`cgroupRoot: /kubelet`). `status.qosClass` is set by the apiserver at create time and cannot change on resize. | Agent discovers the kubepods root once at startup and computes paths (D6). |

## 3. Design decisions

| # | Decision | Why (alternatives rejected) |
|---|----------|-----------------------------|
| D1 | **Namespaced `IOLimiter`**: `podSelector` (required; `{}` = all pods in the namespace) + `volumes[]` identified by **pod volume name** (`pod.spec.volumes[].name`) + `limits`. Bandwidth is `resource.Quantity` (`10Mi`), IOPS is `int64`. | Mount path is per container and meaningless at the pod cgroup level. PVC claim name differs per StatefulSet replica (`data-web-0`), so one CR couldn't target a StatefulSet. The volume name matches the sts `volumeClaimTemplates[].name` and deployment templates. |
| D2 | **Overlapping IOLimiters → per-field minimum** (most restrictive), per volume, order-independent. `PodIOLimit.spec.volumes[].sources` lists the contributing limiters. No conflict condition. | Deterministic, and a limiter can only tighten, never loosen. "Oldest wins" silently disables a newer, stricter limiter and makes the result depend on creation order. |
| D3 | **Annotation mode dropped.** No shim. README gets an annotation→IOLimiter migration table. | A shim means two parsers and two code paths in the privileged component, for a v1alpha1 rewrite whose only known user is the author. The legacy `path=` model (mount path + hostPath) doesn't fit D1/D6 anyway. |
| D4 | **Architecture: controller compiles, agent enforces.** The controller (Deployment, leader-elected) watches Pods, PVCs and IOLimiters and writes one `PodIOLimit` per limited pod (spec = fully resolved desired state: node, pod UID, QoS, kubelet dir name, int64 limits). The agent (DaemonSet) watches **only** the `PodIOLimit`s whose `spec.nodeName` is its node (CRD selectable field, field-selected watch) and writes their `status`. | Least privilege: the agent (the component with host access) gets **no** read access to Pods, PVCs or Secrets, and no write access to any spec. API load: N agents × one field-selected watch on a small CRD, instead of N cluster-wide Pod+PVC+IOLimiter caches (option (a)). Per-pod objects (vs one per node) give per-pod status for debugging (`kubectl get pil`) and no single 1.5 MB object per node rewritten on every pod churn. GC of a `PodIOLimit` whose pod is gone is **not** left to Kubernetes' ownerRef GC alone: the D9 finalizer means GC's own delete only starts the release handshake, and D30 (fix batch, 2026-09-23) made the controller itself watch and reconcile every `PodIOLimit` directly (not only via ownerRef), so an orphan with no ownerRef at all -- which ownerRef GC could never have reached in the first place -- still gets released and deleted. Cost: one extra hop (~sub-second) between pod scheduling and enforcement. |
| D5 | **Pod-level cgroup, not per-container.** No `containers` field. Applied regardless of container readiness. | The pod cgroup exists before any container starts and outlives container restarts, so there's nothing to redo on restart and no unthrottled window after a crash-loop restart. The volume belongs to the pod, so a shared budget across its containers is the natural meaning. It covers init, sidecar and ephemeral containers too, which the legacy code ignored. The legacy code skipped `!Ready` containers, a feedback loop (throttled app → unready → unthrottled → ready → throttled). Per-container would need container-ID tracking, runtime-specific scope names (`cri-containerd-`/`crio-`/cgroupfs) and a re-apply race on every restart. |
| D6 | **Agent resolution with no PIDs and no tree walk.** Cgroup: discover the kubepods root once at startup (candidates `kubepods.slice`, `kubelet.slice/kubelet-kubepods.slice`, `kubepods`, `kubelet/kubepods`; override with `--kubepods-cgroup`), then compute the pod path from UID+QoS (K9). Device: host `/proc/1/mountinfo` → mount point under `--kubelet-root-dir` (default `/var/lib/kubelet`) for `pods/<uid>/volumes/*/<dirName>[/mount]` (the **last** matching entry wins, since later mounts shadow earlier ones) → `MAJ:MIN` → K4 whole-disk. The agent **never** takes a path from the spec: it builds every path itself and refuses anything outside the discovered kubepods root. | Deletes the legacy chain (tree walk → scope → first PID → `/proc/<pid>/mountinfo`), which raced container start and depended on the runtime. A forged `PodIOLimit` can at worst throttle a pod, never `system.slice`. **Rejected: `stat(/proc/<pid>/root/<mountPath>).st_dev`** (parallel review's suggestion). It still needs a PID from a container that mounts the volume (the pause container has its own mount ns, and pod cgroups have no procs of their own), so it keeps the start-up race and the container-ID mapping. `/proc/<pid>/root` needs `PTRACE_MODE_READ` on the target, i.e. `CAP_SYS_PTRACE` for any non-root app, which breaks D14's drop-ALL. It gives the same anonymous `0:N` for btrfs anyway. Host mountinfo + kubelet dir is PID-free and exact. |
| D7 | **User decision (2026-09-23): a pod-level limit is acceptable where a per-volume one isn't possible.** The limit goes on the pod cgroup, one `write()` per device. Volumes on different devices each get their own rule. Volumes on the same device share it: one rule, the most restrictive value per key, and each such volume shows `state: Applied, reason: SharedDevice`, with a message naming the other volumes and the effective rule. io.max has **no cross-device aggregate**: the pod is limited *per device*, never as a total across devices. README documents this. | K7: the kernel tells cgroups and devices apart, not mounts. |
| D8 | **Ownership = `PodIOLimit.status.devices`, written before the kernel.** Before writing a new or changed rule, the agent `Status().Update`s the device into `status.devices` with `state: Pending` (optimistic-locked: on conflict it requeues and does **not** touch the kernel). After the write is verified: `Applied`. The agent resets only devices in `status.devices` that are no longer desired. No startup wipe of the node. | "Only reset what we wrote", with no window where a rule exists in the kernel but not in the record, and it survives agent restarts without local state. The legacy `resetAllRules` wiped every non-`max` rule on every container on the node, including rules other tools wrote. |
| D9 | **Deletion handshake via finalizer `storage.maurice.fr/io-max-reset`**, owned by the **controller**. The controller adds it at create time. When a `PodIOLimit` is being deleted, the agent resets every device in `status.devices` (a missing cgroup counts as reset) and writes `status.devices: []` + `Ready=False/Released`. The controller then removes the finalizer (`MergeFrom` **with optimistic lock**). The controller also removes it without waiting for the agent when the pod with that UID no longer exists or is terminal (its cgroup is gone, so there is nothing to reset: this covers dead nodes). | The agent needs no write on the main resource, only `status`. Optimistic lock + D8's intent-first ordering close the race "controller sees empty status from a stale cache while the agent is mid-apply". |
| D10 | **No finalizer on `IOLimiter`.** Deleting one → the controller recomputes every pod whose `PodIOLimit` lists it in `sources` (field index), then patches or deletes those `PodIOLimit`s → D8/D9 reset the kernel. | The `PodIOLimit` records already say who is affected, so a finalizer would only add a way to get stuck. |
| D11 | **Agent loop:** event-driven (controller-runtime, one reconciler), `RequeueAfter` 2s while `Pending` (cgroup/mount not there yet), otherwise `--resync-period` (default 60s, jittered) to detect drift: re-read `io.max`, rewrite if it differs, count `drift_corrections_total`. Every write is followed by a read-back compare. Status is written only when it semantically changed (ignoring condition timestamps). | Replaces the 5s poll. Drift covers anything else rewriting pod-slice `io.max` (e.g. systemd re-realizing the slice). |
| D12 | **One image, subcommand dispatch** `manager agent …` via `os.Args[1]` before `flag.Parse`, with its own `flag.FlagSet`. **Not cobra.** | Matches the reference (`manager sync`) and the kubebuilder scaffold's stdlib `flag` + zap flags (Rule 11). Cobra on the manager fights the scaffold for no gain. One image = the two can't skew in version. |
| D13 | **No admission webhooks, no cert-manager.** Validation = CRD schema + CEL. Optional hardening = `ValidatingAdmissionPolicy` (CEL, no certs) in C7. | Nothing needs mutation or cross-object checks at admission. Leaving out cert-manager removes an install dependency. |
| D14 | **Agent is not `privileged`.** Pod: `runAsUser: 0`, `runAsNonRoot: false`, `seccompProfile: RuntimeDefault`, no `hostPID`, no `hostNetwork`. Container: `privileged: false`, `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, `readOnlyRootFilesystem: true`. Volumes: hostPath `/sys/fs/cgroup` → `/host/cgroup` **rw** (**not** under `/sys`, so AppArmor's `deny /sys/fs/cg…` rules don't match; narrowed to the kubepods subtree only by D28); hostPath `/proc/1/mountinfo` → `/host/proc/1/mountinfo` **ro**, `type: File` (narrowed by the F1 fix below — only this one file is ever read). `/sys/dev/block` is read from the container's own read-only sysfs (block devices aren't namespaced). The namespace carries `pod-security.kubernetes.io/enforce: privileged` (hostPath is forbidden by `baseline`). `priorityClassName: system-node-critical`, tolerate everything. | cgroup interface files are root-owned 0644. uid 0 passes DAC as owner, so no capability is needed (the `nsdelegate` restriction only covers the writer's own cgroupns root). `/proc/<pid>/mountinfo` is 0444 with no ptrace check. **Both claims are from memory and are verified by the C3 e2e.** SELinux-enforcing nodes (CRI-O/OpenShift) will need `seLinuxOptions.type: spc_t`: documented, untested. |
| D14-F1 | **Fix (Fable review F1, 2026-09-23): the `/proc` hostPath is a single File (`/proc/1/mountinfo`), not a Directory bind-mount of the whole host `/proc`.** A `Directory`-type mount of the whole host `/proc` let a uid-0 process in the agent container reach `/host/proc/1/root` -- a magic symlink into host PID 1's own root mount (`fs/proc/base.c: proc_pid_get_link` → `ptrace_may_access(task, PTRACE_MODE_READ_FSCREDS)`, which passes without any capability when the caller's uid/gid equal the target's, here uid 0 == PID 1's uid 0, and the target is dumpable, which systemd is) -- and from there read or write anything uid 0 owns on the node's real rootfs via ordinary DAC (`/etc/shadow`, kubelet's client certificate, every Secret volume mounted on the node), with no `CAP_SYS_PTRACE`, no `hostPID`, no `privileged`. D14's own "`/proc/<pid>/mountinfo` is 0444 with no ptrace check" claim is true and unaffected -- it never considered the *directory* mount's magic-symlink surface. Fix: mount only `/proc/1/mountinfo` (`type: File`) at the same container path the agent's `--proc-root` default already composes (`<proc-root>/1/mountinfo`) -- `proc_pid_mountinfo`/`mounts_open_common` (the file D14 already vouches for) has no ptrace gate, so this needed **no agent code change**. Updated blast radius (also corrects the README's "throttle a pod at worst" claim, which was false while this was open): a compromised or buggy agent can still write any cgroup file under the kubepods subtree on its own node (throttle or freeze any pod scheduled there) and read/forge any `PodIOLimit`'s status cluster-wide (RBAC can't express a per-node field selector), but can no longer reach the host rootfs, `system.slice`, or any other node. Verified by a negative e2e (`test/e2e/hardening.TestAgentProcMountNarrowed`): `/host/proc/1/root` does not exist, and `/host/proc`/`/host/proc/1` contain nothing beyond the one mounted file. |
| D15 | **Two RBAC roles from markers.** `make manifests` runs controller-gen twice: `rbac:roleName=manager-role paths=./internal/controller/...;./cmd/...` and `rbac:roleName=agent-role paths=./internal/agent/...` → `config/agent/rbac`. See §7. | A single controller-gen pass over `./...` would merge the agent's markers into the manager's role and the other way round. |
| D16 | **Caches.** Controller: Pods and PVCs cluster-wide (needed as watch sources), with a cache `TransformFunc` that strips `managedFields`. Field indexes: `PodIOLimit` by `spec.podName` and by `spec.volumes[].sources`; `Pod` by PVC claim name. Agent: cache `ByObject{PodIOLimit: field spec.nodeName=$NODE_NAME}`, and **no other type is ever read through the client** (a stray `Get` would lazily start a cluster-wide informer that RBAC forbids). | Lessons from the reference: narrow caches, cached reads need list/watch RBAC. |
| D17 | **Writes.** Nobody full-`Update`s a CR. Controller: `PodIOLimit` spec/metadata via `client.MergeFrom` (finalizer removal via `MergeFromWithOptimisticLock`), `IOLimiter` via `Status().Update` only (conflict → requeue at V(1), not an error). Agent: `PodIOLimit` via `Status().Update` only. | Reference gotcha (Quantity/Duration re-encoding vs CEL `self == oldSelf`). A status update carries resourceVersion, so it gets optimistic locking for free (needed by D8). |
| D18 | `PodIOLimit` name = `<podName>-<podUID[:8]>` (pod name truncated so the total is ≤ 253). ownerRef → Pod (`controller: true`, `blockOwnerDeletion: false`). Spec identity fields (`nodeName`, `podName`, `podUID`, `qosClass`) are immutable (CEL). | A recreated StatefulSet pod (same name, new UID) never collides with the previous pod's `PodIOLimit`, which may still be held by D9. |
| D19 | **Supported volume sources:** `persistentVolumeClaim` and generic `ephemeral` (claim `<pod>-<vol>`) with `volumeMode: Filesystem` and a Bound PVC (dirName = PV name), and inline `csi` (dirName = volume name). Anything else (hostPath, emptyDir, configMap, block-mode PVC …) → reported on the IOLimiter as `UnsupportedVolumeType` and not put in the `PodIOLimit`. Unbound PVC → pod skipped for that volume until the PVC watch fires. **Fable review F4 (fix batch, 2026-09-23): a Bound PVC backed by a `hostPath`-source `PersistentVolume` (e.g. k3s/Rancher `local-path-provisioner`) is also `UnsupportedVolumeType`, naming the PV** -- kubelet's hostPath PV plugin never mounts it under `pods/<uid>/volumes/` (K8), so the agent could never resolve a device for it and it would otherwise poll `Pending` forever with no indication why. A `local`-source PV is deliberately *not* rejected: kubelet mounts it under `pods/<uid>/volumes/kubernetes.io~local-volume/<name>` like any other plugin (`internal/mountinfo.FindKubeletVolume` matches structurally, not by plugin name), which is exactly what the kind e2e's storage class exercises successfully. This needs the controller to read the PV: `persistentvolumes: get,list,watch` (RBAC, `manager-role`), cached cluster-wide with the same `managedFields`-stripping transform as Pods/PVCs (D16), watched with a predicate on the claim binding and the volume source (F9). A PV not yet in cache is treated as "nothing to reject" (fails open, matching the existing PVC-not-found stance) rather than blocking resolution on cache lag. | K8. hostPath was the legacy test fixture (OQ4). |
| D20 | **Status split.** Controller-side problems (volume missing in pod, unsupported type, PVC unbound) are computed by the same pure function the controller uses to build the spec, and surface only on `IOLimiter.status`. Node-side problems (cgroup/mount missing, major 0, kernel rejected) go on `PodIOLimit.status.volumes[]` and are aggregated into `IOLimiter.status`. `IOLimiter` `Ready` is `True` iff every matched pod's `PodIOLimit` has `observedGeneration == generation` and `Applied` for every volume this limiter contributes to. When an agent is down, its pods stay `Pending`, which is visible. | Each problem shows up exactly once, close to where it's fixable. |
| D21 | Tests: plain `go test` + testify everywhere (unit, envtest, e2e), no ginkgo. Logging = controller-runtime `logr` in **both** manager and agent (both run a controller-runtime manager). Lint = golangci-lint + the `logcheck` plugin, copied from the reference. `CGO_ENABLED=0` on every `go` invocation. | Reference D12 + Rule 11 (framework convention over the global slog default). |
| D23 | **Refuse the node's root / kubelet-dir device by default** (`Unsupported/SharedRootDevice`), opt in per node with agent flag `--allow-root-device`. Detection: device of mountinfo's `/` entry and of the entry holding `--kubelet-root-dir`, both mapped to whole disk (K4). | Legacy `path=/` guard was decorative: any local-path/emptyDir volume sits on the root disk anyway. K7: throttling that disk throttles the pod's rootfs too, and under ext4 `data=ordered` other cgroups' `fsync` can queue behind the throttled pod's writeback (node-wide priority inversion). Fail safe; k3s/local-path users opt in knowingly. (Promoted from open question: both design passes agreed.) |
| D24 | **Agent liveness = loop progress, not ping.** `healthz` fails if (a) the self-check (cgroup root + kubepods dir present, `/host/proc/1/mountinfo` readable) hasn't passed within 3 × `--resync-period`, or (b) the cache holds ≥ 1 `PodIOLimit` and no reconcile has *completed* (success or handled error) within 3 × `--resync-period` (every object requeues each period, so silence means a wedged worker). A permanently `Failed` volume is not unhealthy. | A restart is harmless (D8: no wipe, rules persist in the kernel), so liveness can be strict. Ping-only liveness would keep a wedged agent "healthy" forever. |
| D25 | **NRI plugin / OCI blockIO: backlog, not v1.** | Both would apply limits at container create and close the gap between pod start and the first agent reconcile. But NRI needs containerd ≥ 1.7 with NRI enabled (off by default before 2.0) or CRI-O with NRI configured, and it acts per container at create time: it can't retrofit running pods, doesn't see CR changes, and knows nothing about PVC→device mapping at create. OCI blockIO classes are static node config. Both are complements to this design, not replacements. Revisit if the start-up window is measured to matter. |
| D26 | **User requirement (2026-09-23): logging is first-class, per §6.3.** Stack = controller-runtime `logr`/zap in both processes (`--zap-log-level`, `--zap-encoder`, `--zap-devel`, `Development: true`, `Level: info`, exactly as the reference `cmd/main.go`). Consistent literal keys (logcheck lint). Info = every state change + every mutating action (API write, kernel write/reset). V(1) = waits, requeues, no-ops. A per-reconcile logger carries `pod=<ns>/<name>` (agent + PodReconciler) or `iolimiter=<ns>/<name>` (IOLimiterReconciler), so `kubectl logs … \| grep <ns>/<name>` reads as a narrative. Events: the controller on `IOLimiter`, the agent on the **Pod**. Errors are returned, not logged-and-returned; conflicts are V(1) requeues. Transitions and events are emitted **after** the status write succeeds. | Debuggability, mirroring the reference's C7 conventions. Pod events from the agent (not the controller) because only the agent knows the exact moment and cause; the price is `events: create,patch` in the agent role. It needs no pod read, since the event reference is built from `spec.podName/podUID`. |
| D22 | **Scaffold in place**, legacy kept compiling until C8: `cmd/k8s-blkio-limiter`, `internal/limiter`, `charts/`, `dev/`, the old workflows. Reused ideas (rewritten, not moved): mountinfo parsing, `io.max` parsing, fs-fixture test style. Discarded: tree walk, PID/container-ID resolution, startup wipe, annotation parsing, multi-line write. | Legacy stays deployable by hand until the new path is proven on kind. |
| D27 | **C7 hardening: two `ValidatingAdmissionPolicy`s. DELETE is *not* restricted (revised 2026-09-23, post-C7 fix batch).** `podiolimits` (main resource) CREATE/UPDATE only from `system:serviceaccount:<ns>:<controller SA>`. `podiolimits/status` UPDATE only from `system:serviceaccount:<ns>:<agent SA>`, **and** only when `request.userInfo.extra['authentication.kubernetes.io/node-name'][0] == object.spec.nodeName` (OQ2's reason for the 1.32+ minimum). Both shipped active by default (kustomize `config/vap`, wired unconditionally into `config/default`; Helm `admissionPolicy.enabled: true`, default on) -- "optional" in D13 means "no cert-manager/webhook dependency", not "opt-in install": a hardening control that ships off by default protects nobody. `apiVersions: ["*"]`, not pinned to `["v1alpha1"]` (fixed in the same batch): a pinned list silently stops protecting the resource the day a new version is served and nobody remembers to update this file; `"*"` matches every version currently served, same semantics as an admission webhook rule, and `test/e2e/hardening.TestVAPPolicies_MatchAllServedVersions` proves both VAPs actually cover every version the CRD serves. **DELETE reversal:** C7 originally restricted DELETE identically to CREATE/UPDATE ("a tenant who can delete a PodIOLimit they don't own forces the D9 release handshake on it, silently un-throttling another tenant's pod"). That reasoning didn't account for two things found in production use: (1) RBAC already gates who may delete a `PodIOLimit` at all (§7: no tenant helper role grants `podiolimits` delete), so the "tenant" the original rationale worried about never had RBAC delete to begin with -- the VAP was blocking a request RBAC would already have refused; a manual tenant deletion, if it ever happened, still goes through the D9 finalizer/agent-reset handshake regardless of who issued the delete, so the "silent un-throttle" framing was wrong -- the reset always happens, only the choice of *when* was taken from the controller, which is not a security property. (2) Restricting DELETE also blocked two identities that must always be able to delete: a cluster-admin doing manual cleanup ("forbidden: ValidatingAdmissionPolicy 'k8s-blkio-limiter-podiolimit-write-restricted'" on a routine `kubectl delete podiolimit`), and, worse, Kubernetes' own `system:serviceaccount:kube-system:generic-garbage-collector` running ownerRef cleanup on pod deletion and the namespace controller's own sweep on namespace deletion -- with the controller down, a namespace containing a limited pod could hang `Terminating` indefinitely, since GC's delete of the `PodIOLimit` itself was denied at admission, not just RBAC-gated. CREATE/UPDATE stay controller-SA-only: those are what can forge or alter a rule outright, and neither Kubernetes' own controllers nor any tenant RBAC role needs to issue either. Verified on kind (`test/e2e/hardening`): `kubectl --as` a user holding real RBAC create on `podiolimits` is still denied by the VAP on CREATE/UPDATE, not RBAC (proves the VAP is the actual blocking layer); an agent-SA identity impersonating the wrong node's `node-name` extra is denied on a status write; an ambient cluster-admin identity **can** delete a `PodIOLimit`, and the finalizer still resets `io.max` first when the pod exists (`TestVAP_AdminCanDeletePodIOLimit`); the real, unmodified controller and agent keep working (`test/e2e/agent` and `test/e2e/operator` stay green under both VAPs, once their harness itself creates/patches hand-crafted `PodIOLimit`s via a controller-SA-**impersonating** client, `harness.ControllerClient` -- the suite "plays the controller" by hand per C3, so under D27 it now has to authenticate as the controller too, not just act like one; `DeletePodIOLimit` no longer needs to impersonate the controller SA since DELETE is unrestricted, but is left routed through `ControllerClient` anyway for parity with the rest of the suite's writes). | RBAC alone (§7) can't express "only the controller may CREATE/UPDATE"; a `PodIOLimit`'s *contents* (the rule) are the security control that needs admission-time protection, not its *existence* -- RBAC plus the D9 finalizer already make deletion safe and controlled, and Kubernetes' own garbage collection and namespace lifecycle must never be blocked by a project-specific admission policy. |
| D30 | **Orphan `PodIOLimit` GC (fix batch, 2026-09-23): a `PodIOLimit` with no ownerRef (or one pointing at a dead pod/UID) must still be reconciled and released.** `PodReconciler` previously only saw a `PodIOLimit` through `Owns()` (ownerRef-driven) or via an `IOLimiter`/PVC event mapping to a live pod -- a hand-crafted or leftover orphan with no ownerRef was never enqueued at all. Fixed by replacing `.Owns(&PodIOLimit{})` with a direct `Watches(&PodIOLimit{}, ...)` mapped by `(namespace, spec.podName)` (the existing `spec.podName` field index, D16) regardless of ownerRef, so *every* `PodIOLimit`'s own create/update event enqueues its target pod's reconcile key -- including one the controller never created. Pre-existing orphans (present before the controller starts) are caught the same way: controller-runtime's informer cache emits a synthetic Create event for every object still present at the start of each watch/resync cycle, and the manager's cache `SyncPeriod` (`cmd/main.go`, previously unset/default) is now explicitly bounded (10m) as a second, independent guarantee -- not relying solely on "the watch never misses an event" for a resource whose whole purpose is a security-relevant ceiling. Once enqueued, the *existing* D9 dead-pod path in `Reconcile` (step 2: any `PodIOLimit` in the by-podName list whose UID doesn't match the live pod, or where the pod doesn't exist) already deletes and releases the finalizer directly, without waiting on the agent -- correct for an orphan, since there is no live cgroup to reset. **Race, chosen and justified:** a `PodIOLimit` whose target pod was *just* created can lose the race between the Pod informer and the PodIOLimit informer at controller startup (the PodIOLimit's own watch syncing before the Pod cache has the matching pod), which would otherwise misidentify a brand-new, legitimate object as an orphan and delete it. Chosen fix: **fall back to a live, uncached read** (`mgr.GetAPIReader()`, wired into `PodReconciler.APIReader`) whenever the cached client reports the pod missing, before concluding it's actually gone -- not the age-based alternative (skip orphan cleanup for anything younger than N seconds), because an age threshold either (a) is short enough to still race a slow-starting Pod informer under load, or (b) is long enough to leave a genuine orphan (the reported bug: a stale `PodIOLimit` with a bogus pod UID) sitting around for that whole window before GC, which is exactly the bug being fixed. A live read costs one extra apiserver round-trip only on the already-rare "pod not in my cache" path, and is exact rather than probabilistic. Unit test (fake client, a `PodIOLimit` present but its pod deliberately absent from the fake's object set entirely, standing in for "not yet cached") and envtest coverage (`TestPodReconciler_OrphanPodIOLimit_NoOwnerRef_Released`, `TestPodReconciler_OrphanPodIOLimit_LiveReadAvoidsRace`). | An orphan `PodIOLimit` is a live kernel-level throttling ceiling with nothing left to justify it; leaving it un-reconciled (the reported bug) is worse than the small cost of one extra live read on an already-uncommon path. |
| D31 | **`VolumeNotFound` is a warning, not a failure** (Fable review F11, coordinator decision 2026-09-23). A pod matched on ≥1 listed volume counts as Applied when every volume it *has* is Applied; volumes the limiter lists but the pod lacks are reported as a `VolumeNotFound` note in the IOLimiter status message + one warning event, without failing the pod. | §6.2 (matched = ≥1 listed volume present) and D20 contradicted each other; "Failed" while the rule is live in the kernel is wrong under either reading. One limiter covering `data`+`wal` must work on a Postgres that only has `data`. |
| D32 | **Cache `SyncPeriod` removed** (F17, supersedes that part of D30). A resync is a local cache replay: no apiserver relist, no recovery from missed watch events, just a full reconcile fan-out every period. Orphans that predate a controller start are still caught by the initial List (every cached PodIOLimit is enqueued at start) and by the `spec.podName`-mapped watch. | Cost with no benefit once the premise is corrected. |
| D33 | **Uninstall = documented procedure + VAP carve-out, no Helm hook** (F3). The write VAP keeps CREATE and **spec** changes controller-only but allows metadata-only UPDATEs (`object.spec == oldObject.spec`) from anyone RBAC permits, so a cluster-admin can strip a stuck finalizer. README gets an "Uninstall" section: delete IOLimiters → wait for PodIOLimits to be gone → uninstall; plus stuck-finalizer recovery. | A pre-delete hook Job adds an image, RBAC and a failure mode to every uninstall for a problem a 3-step procedure solves (Rule 2). |
| D34 | **Refused-device set generalised** (F12): agent flag `--protected-paths` (default `/,/var/lib/kubelet,/var/lib/containerd,/var/lib/containers`) replaces the root+kubelet-dir pair of D23; the device (whole disk) holding any of these paths is refused unless `--allow-root-device`. Missing paths are ignored. | The container-runtime storage disk has the same fsync-inversion risk (K7) as the root disk. |
| D35 | **An invalid limit freezes, it does not loosen** (chaos test S7, coordinator decision 2026-09-24). When an IOLimiter has any `InvalidLimit` issue, the controller does **not** patch the existing `PodIOLimit`s of the pods it matches: their last-applied spec (and so the kernel rule) is kept, and the IOLimiter reports `InvalidLimit` with "previous limits kept". Pods newly matched while the limiter is invalid get no contribution from it (nothing to keep). Once the limiter is fixed, normal reconciliation resumes. | Observed on kind: editing `writeBytesPerSecond` 5Mi→`100m` turned `wbps` into `max` within seconds: a typo in an admin-imposed limit silently removed the throttle. Fail-safe for a limiter means "keep the last good ceiling". |
| D36 | **D35 revised: per-limiter contributions, not a whole-volume floor** (review of D35, 2026-09-24). `PodIOLimit.spec.volumes[]` records each source limiter's own contribution (`contributions: [{source, limits}]`); the effective `limits` is the per-field minimum of the contributions (D2). An IOLimiter with an `InvalidLimit` reuses **its own** last contribution from the existing spec (never loosens, never ratchets other limiters' values); a limiter deleted or no longer matching simply drops its contribution. `sources` is derived from `contributions`. **No upgrade path needed:** `v1alpha1` was never released, so no `PodIOLimit` predates the `contributions` field -- there is nothing to backfill. If a future API version needs one, backfill `contributions` from the older `sources`/`limits` pair (one synthetic contribution per source, using the merged `limits` as its own -- lossy but consistent with pre-D36 semantics). | The whole-volume floor ratcheted: relaxing a valid limiter had no effect while another limiter stayed invalid. |
| D37 | **Selector size bounds + selector cache** (security review, DoS). `podSelector` bounded (≤16 matchExpressions, ≤64 values each, ≤16 matchLabels, values/keys length-capped) via CEL if within the cost budget, otherwise controller-side as `InvalidSelector`. **Updated (2026-09-24, fix batch): `InvalidSelector` freezes, like D36's `InvalidLimit`, it does not loosen.** A limiter that already contributes to a pod's `PodIOLimit` keeps that contribution if its own selector goes invalid and it's still a source in the pod's existing spec (`limitersForPod`, `internal/controller/selector.go`, adds it back into the set fed to `desired.Compute` even though it no longer selector-matches); its volume rows still apply normally (or freeze via D36 if also `InvalidLimit`). A limiter that's deleted, or whose selector is valid but simply no longer matches, is still genuinely dropped -- `mapIOLimiterToPods` already re-enqueues the affected pod via the PodIOLimit's own `sources` index in both cases. The controller parses each limiter's selector once per generation (cache keyed by UID+generation) instead of per pod event. README recommends a `count/iolimiters.storage.maurice.fr` ResourceQuota when the create grant goes to untrusted users. | Originally "limiter contributes nothing" on `InvalidSelector` -- same D35 bug class for selectors: `listMatchingLimiters` excluding an invalid-selector limiter dropped its contribution from `desired.Compute`'s input entirely, silently removing an already-applied throttle on a bad selector edit. A tenant with IOLimiter create could also OOM/CPU-starve the cluster-wide controller (the bounds themselves, unchanged). |
| D38 | **Evasion is surfaced, not prevented** (security review). IOLimiter is a ceiling on what it selects, not an enforcement boundary against the namespace's own editors (rename a volume / relabel a pod escapes it). The IOLimiter status reports `selectedPodsWithoutVolumes` (pods matching the selector that have none of the listed volumes) plus a Warning event, so an admin sees the escape instead of `NoMatchingPods`. README states the model plainly, and that one filesystem per PV is required for multi-tenant isolation (K7 fsync inversion on shared filesystems is documented, not detected). | Honest trust model; detection of shared-filesystem PVs is out of scope for v1alpha1. |
| D39 | **Supply chain + input hardening** (security review). CI actions pinned by commit SHA; kind download sha256-verified; Dockerfile base images pinned by digest; images signed with cosign keyless in the publish job; README tells users to pin a released tag/digest. `podUID` must be a UUID (CRD pattern + agent check), `kubeletDirName` a DNS-1123 subdomain, cgroup confinement requires a path strictly below the kubepods root; status VAP uses `has(request.userInfo.extra)`. README: the operator namespace is cluster-admin-equivalent. | Defence in depth; each item is cheap. |
| D28 | **Agent cgroup mount narrowed to the kubepods subtree**, not all of `/sys/fs/cgroup` (D14 update). `config/agent/agent.yaml` and the Helm chart (`hack/helm-postprocess.sh`, `agent.cgroupHostSubtree` value) bind-mount only `/sys/fs/cgroup/<subtree>` (default `kubelet.slice/kubelet-kubepods.slice`, kind's layout) at the **same relative path** under `/host/cgroup` (i.e. `/host/cgroup/kubelet.slice/kubelet-kubepods.slice`), not flattened onto `/host/cgroup` itself: D6's discovery (`cgroup.DiscoverKubepods`) still finds it via its existing candidate search against `--cgroup-root=/host/cgroup` (unchanged default) with **zero code changes**, since nothing else exists under `/host/cgroup` for it to find instead. A typical non-kind systemd node overrides the subtree to `kubepods.slice` (cgroupfs: `kubepods` or `kubelet/kubepods`); documented in the README and the chart's `values.yaml` comment. **Preflight had to change** (anticipated by the chunk spec, not guessed): `cgroup.controllers` no longer exists at `--cgroup-root` itself (an empty mountpoint-stub directory once only the subtree is mounted), only inside the actual mounted subtree -- `agent.go`'s startup check and `runSelfCheckLoop`/`selfCheckPasses` (D24) now stat it at the *discovered* `layout.Base` instead, which is still an exact cgroup2 proof: cgroup2 exposes `cgroup.controllers` in every directory of the hierarchy, not just the real mount point (confirmed live on kind, not assumed). Verified directly on kind via the agent's own mount namespace (`/proc/<pid>/root/host/cgroup` from the node, the image is distroless so there's no in-container shell to `kubectl exec` into): `io` is enabled in the mounted subtree's `cgroup.subtree_control`, `io.max` writes/reads still work (the full e2e suite stays green), and neither `system.slice` nor `kubelet.service` exist anywhere under the agent's view (`test/e2e/hardening.TestAgentCgroupMountNarrowed`). | A compromised/buggy agent with the old full-tree mount could reach `system.slice`/`kubelet.service` and cause a node-wide DoS via `memory.max`, `cgroup.freeze` or `cgroup.procs` there -- D14's own least-privilege reasoning, extended past RBAC to the host mount surface. |
| D29 | **Helm PSA gap (flagged in C6): optional chart-managed `Namespace`, off by default.** `namespace.create: false` by default (unchanged install flow: `helm install --create-namespace`, then label the namespace yourself, as C6 documented); `namespace.create: true` makes the chart template its own `Namespace` object with the same PSA `privileged` labels `config/default`'s kustomize `namespace_psa_patch.yaml` adds, so a fresh PSA-enforcing-cluster install needs neither `--create-namespace` nor a manual label step. **Not plugin-native, checked not guessed:** the kubebuilder `helm/v2-alpha` plugin drops `config/manager/manager.yaml`'s `Namespace` object entirely, verified by running the plugin alone (no postprocess) both before and after moving the PSA labels directly onto that `Namespace` object (replacing the JSON6902 patch) -- neither produced a namespace template anywhere in the chart, so there's no plugin-native path to prefer over `hack/helm-postprocess.sh` authoring `templates/namespace.yaml` itself. Off by default (not the simplest *behaviour*, but the simplest *migration*): flipping the default would silently break `helm install --create-namespace` for every existing install by fighting over the same namespace object between Helm's own creation and this chart's template. | A chart-managed `Namespace` is simpler and more robust than a pre-install hook (hooks run outside the normal apply/diff lifecycle and can leave the labels applied but the release install failed, or vice versa) and strictly better than leaving it fully manual. |

## 4. API (`api/v1alpha1/`)

### 4.1 `IOLimiter`

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=iol
// +kubebuilder:printcolumn:name="Matched",type=integer,JSONPath=".status.matchedPods"
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=".status.appliedPods"
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=".status.failedPods"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type IOLimiter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec   IOLimiterSpec   `json:"spec"`
	Status IOLimiterStatus `json:"status,omitempty"`
}

type IOLimiterSpec struct {
	// PodSelector selects pods in this namespace. {} selects every pod.
	// +required
	PodSelector metav1.LabelSelector `json:"podSelector"`

	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Volumes []VolumeLimit `json:"volumes"`
}

type VolumeLimit struct {
	// Name of the pod volume (pod.spec.volumes[].name).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name   string   `json:"name"`
	Limits IOLimits `json:"limits"`
}

// IOLimits caps the pod's IO on the whole-disk device backing the volume
// (cgroup v2 io.max). Unset = unlimited.
// +kubebuilder:validation:XValidation:rule="has(self.readBytesPerSecond) || has(self.writeBytesPerSecond) || has(self.readIOPS) || has(self.writeIOPS)",message="at least one limit must be set"
type IOLimits struct {
	// +optional
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).isInteger() && quantity(string(self)).compareTo(quantity('1Ki')) >= 0 && quantity(string(self)).compareTo(quantity('1Pi')) <= 0",message="must be a whole number of bytes between 1Ki and 1Pi"
	ReadBytesPerSecond *resource.Quantity `json:"readBytesPerSecond,omitempty"`
	// +optional
	// +kubebuilder:validation:XValidation:rule="<same as above>",message="must be a whole number of bytes between 1Ki and 1Pi"
	WriteBytesPerSecond *resource.Quantity `json:"writeBytesPerSecond,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=4294967295
	ReadIOPS *int64 `json:"readIOPS,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:validation:Maximum=4294967295
	WriteIOPS *int64 `json:"writeIOPS,omitempty"`
}

type IOLimiterStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Scheduled, non-terminal pods selected by podSelector that have at least one listed volume.
	MatchedPods int32 `json:"matchedPods"`
	AppliedPods int32 `json:"appliedPods"`
	FailedPods  int32 `json:"failedPods"`
	// Ready: True/AllApplied | True/NoMatchingPods | False/Pending | False/Failed.
	// The message names up to 5 failing pods: "<pod>: volume <v>: <reason>: <msg>".
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

CEL on an int-or-string Quantity (`string(self)` for the integer form) must
be proven by envtest in C0. If `quantity()` won't compile against
`x-kubernetes-int-or-string`, the fallback is `isQuantity(string(self))` plus
the same controller-side check. Don't switch to plain `int64` bytes: `10Mi`
readability is part of the brief.

```yaml
apiVersion: storage.maurice.fr/v1alpha1
kind: IOLimiter
metadata: {name: postgres-data, namespace: apps}
spec:
  podSelector:
    matchLabels: {app: postgres}
  volumes:
    - name: data                 # sts volumeClaimTemplates[].name
      limits:
        readBytesPerSecond: 20Mi
        writeBytesPerSecond: 10Mi
        readIOPS: 500
        writeIOPS: 200
    - name: wal
      limits:
        writeBytesPerSecond: 5Mi
```

### 4.2 `PodIOLimit` (internal)

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pil
// +kubebuilder:selectablefield:JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Pod",type=string,JSONPath=".spec.podName"
// +kubebuilder:printcolumn:name="Node",type=string,JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].reason"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type PodIOLimit struct { /* TypeMeta, ObjectMeta, Spec, Status */ }

// +kubebuilder:validation:XValidation:rule="self.nodeName == oldSelf.nodeName && self.podName == oldSelf.podName && self.podUID == oldSelf.podUID && self.qosClass == oldSelf.qosClass",message="pod identity is immutable"
type PodIOLimitSpec struct {
	NodeName string    `json:"nodeName"`
	PodName  string    `json:"podName"`
	PodUID   types.UID `json:"podUID"`
	// +kubebuilder:validation:Enum=Guaranteed;Burstable;BestEffort
	QOSClass corev1.PodQOSClass `json:"qosClass"`
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	Volumes []PodVolumeLimit `json:"volumes"`
}

type PodVolumeLimit struct {
	Name string `json:"name"` // pod volume name
	// Directory kubelet mounts this volume under (pods/<uid>/volumes/<plugin>/<dir>):
	// the PV name for PVC/ephemeral volumes, the pod volume name for inline CSI.
	KubeletDirName string       `json:"kubeletDirName"`
	Limits         DeviceLimits `json:"limits"` // already merged (D2), nil = max
	// +listType=set
	Sources []string `json:"sources"` // contributing IOLimiter names, sorted
}

type DeviceLimits struct {
	ReadBPS   *int64 `json:"readBPS,omitempty"`
	WriteBPS  *int64 `json:"writeBPS,omitempty"`
	ReadIOPS  *int64 `json:"readIOPS,omitempty"`
	WriteIOPS *int64 `json:"writeIOPS,omitempty"`
}

type PodIOLimitStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	CgroupPath         string `json:"cgroupPath,omitempty"` // relative to the cgroup2 root, informational only
	// Ownership record (D8): every device a rule may have been written for in
	// this pod's cgroup. Entries are added BEFORE the kernel write.
	// +listType=map
	// +listMapKey=device
	Devices []OwnedDevice `json:"devices,omitempty"`
	// +listType=map
	// +listMapKey=name
	Volumes []VolumeStatus `json:"volumes,omitempty"`
	// Ready reasons: Applied | Pending | Failed | Released
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type OwnedDevice struct {
	Device string `json:"device"` // "MAJ:MIN", whole disk
	Rule   string `json:"rule"`   // "rbps=… wbps=… riops=… wiops=…" (all four keys)
	// +kubebuilder:validation:Enum=Pending;Applied
	State string `json:"state"`
}

type VolumeStatus struct {
	Name        string `json:"name"`
	Device      string `json:"device,omitempty"`      // whole disk throttled
	MountDevice string `json:"mountDevice,omitempty"` // from mountinfo, when it was a partition
	// +kubebuilder:validation:Enum=Applied;Pending;Unsupported;Failed
	State string `json:"state"`
	// CgroupNotFound | VolumeNotMounted | NoBlockDevice | SharedRootDevice |
	// IOControllerDisabled | KernelRejected | KernelMismatch | SharedDevice
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}
```

## 5. Architecture

```
            ┌────────────────────────── API server ───────────────────────────┐
 user ────▶ │ IOLimiter (ns)          Pod, PVC                                  │
            └──────┬───────────────────────┬────────────────────────▲─────────┘
                   │ watch                  │ watch                  │ create / MergeFrom patch /
                   ▼                        ▼                        │ delete / finalizer (optimistic)
            ┌────────────────────────────────────────────────────────┴────────┐
            │ controller Deployment  (`/manager`, leader-elected)               │
            │  PodReconciler        pod × IOLimiters × PVC → PodIOLimit.spec    │
            │  IOLimiterReconciler  pods + PodIOLimit.status → IOLimiter.status │
            └──────────────────────────────────────────────────────────────────┘
                                          │ PodIOLimit (ns, 1 per limited pod,
                                          │  ownerRef→Pod, finalizer io-max-reset)
            ┌─────────────────────────────▼──────────────────────▲─────────────┐
            │ watch fieldSelector spec.nodeName=$NODE_NAME         │ Status().Update
            │ agent DaemonSet pod  (`/manager agent`, not privileged)            │
 node N     │   /host/proc/1/mountinfo (ro)  kubelet volume mount → MAJ:MIN      │
            │   /sys/dev/block (own sysfs)   partition → whole disk              │
            │   /host/cgroup (rw)            <kubepods>/…pod<uid>…/io.max        │
            └──────────────────────────────────────────────────────────────────┘
```

Failure modes:
- **Controller down**: existing rules keep being enforced and drift-corrected by the agents. New pods and CR changes wait. `IOLimiter` status is stale.
- **Agent down on node N**: existing kernel rules persist. New or changed `PodIOLimit`s on N stay `Pending` (visible on the `IOLimiter`). Deletions are held by the finalizer until the agent returns, except when the pod itself is gone (D9).
- **Node dead**: pods get deleted → the controller releases the finalizers (D9), and GC does the rest.

## 6. Algorithms

### 6.1 Agent (`internal/agent`), per `PodIOLimit` on this node

Startup: require `/host/cgroup/cgroup.controllers` (cgroup2), discover the
kubepods root (D6) and require `io` in its `cgroup.subtree_control`. `readyz`
fails until these checks pass and the cache has synced. `healthz` per D24.

1. Get from cache. `spec.nodeName != NODE_NAME` → ignore (defense in depth).
2. `podCg := cgroup.PodPath(root, spec.podUID, spec.qosClass)`. If `deletionTimestamp` is set → **Release** (step 11).
3. `podCg` missing → volumes `Pending/CgroupNotFound`, requeue 2s. `io.max` missing in `podCg` → `Failed/IOControllerDisabled`.
4. Parse `/host/proc/1/mountinfo` once. For each volume, find the mount point (D6). Not found → `Pending/VolumeNotMounted`. Major 0 → `Unsupported/NoBlockDevice`. Partition → parent disk (K4). Same disk as host `/` or kubelet root → `Unsupported/SharedRootDevice` unless `--allow-root-device` (D23).
5. Group resolved volumes by disk, merge per field min (D7) → `desired map[dev]Rule` (all four keys).
6. `owned := status.devices`. `stale := owned − desired`.
7. If desired has a device that isn't owned, or whose rule changed: `Status().Update` with `devices = owned ∪ desired` (new or changed → `Pending`). Conflict → requeue, **no kernel write**.
8. For each desired device: read `io.max`. If the line differs, write **one** rule per `write()` (K1), then read back and compare (K6). Error → `Failed/KernelRejected` (errno in the message). Mismatch → `Failed/KernelMismatch`.
9. For each stale device: write the reset line (K3). `ENOENT`/`ENODEV` counts as reset.
10. `Status().Update` (only if changed): `devices` = verified desired (`Applied`) plus desired that failed (still `Pending`: we may have written it), volume states, `Ready`, `observedGeneration`, `cgroupPath`. Requeue `--resync-period` (jittered) or 2s if anything is `Pending`.
11. **Release**: reset every device in `status.devices` (a missing cgroup counts as done). Then `Status().Update` with `devices: []` and `Ready=False/Released`. Never write a non-reset rule for an object that is being deleted.

Agent flags: `--node-name` (default `$NODE_NAME`), `--cgroup-root=/host/cgroup`,
`--proc-root=/host/proc`, `--kubelet-root-dir=/var/lib/kubelet`,
`--kubepods-cgroup` (optional override), `--resync-period=60s`,
`--allow-root-device=false`, plus the scaffold's metrics/probe/zap flags.
Metrics (on the controller-runtime registry):
`iolimiter_agent_owned_devices` (gauge),
`iolimiter_agent_kernel_writes_total{op=apply|reset,result=ok|error}`,
`iolimiter_agent_drift_corrections_total`,
`iolimiter_agent_volume_resolution_total{result=<reason>}`.

### 6.2 Controller (`internal/controller`)

Pure core: `desired.Compute(pod, limiters, pvcLookup) (volumes []PodVolumeLimit, issues []Issue)`.
It is used by both reconcilers, so spec and status can't disagree.

**PodReconciler** (key = pod ns/name; watches Pod, IOLimiter → pods matching
the selector ∪ pods whose `PodIOLimit` has it in `sources`, PVC → pods using the
claim (index), PodIOLimit → its own `spec.podName` (D16 index), **not** just
ownerRef: this is what makes an orphan `PodIOLimit` with no ownerRef -- or one
whose ownerRef points at a pod that's gone -- reach this reconciler at all,
D30):
1. Get the pod (cached read; fall back to a live, uncached read via the
   manager's `APIReader` if the cache reports it missing, D30 -- avoids
   misidentifying a just-created pod not yet in the Pod informer's cache as
   "gone"). List `PodIOLimit`s in the namespace by index `spec.podName`.
2. Each `PodIOLimit` whose `spec.podUID` ≠ the live pod UID (or the pod is
   missing, confirmed by the live read above): delete it if it isn't already
   being deleted, and remove the finalizer (optimistic lock) without waiting
   on the agent -- there is no live cgroup left to reset. This is also the
   path that garbage-collects a genuine orphan (no ownerRef, or a bogus/dead
   pod UID, D30): nothing about this step depends on how the `PodIOLimit`
   came to exist.
3. Pod missing, not scheduled (`nodeName == ""`), or terminal → desired = none. If a `PodIOLimit` exists for this UID: delete it and remove the finalizer directly (terminal/gone ⇒ no live cgroup).
4. `volumes, _ := desired.Compute(...)`. Empty → delete the `PodIOLimit` (finalizer kept: the agent must reset).
5. Otherwise create it (D18 name, ownerRef, finalizer, `app.kubernetes.io/managed-by: k8s-blkio-limiter`), or `MergeFrom`-patch the spec if it differs. If the existing one is being deleted (held by D9) → requeue 2s.
6. For a `PodIOLimit` being deleted whose `status.devices` is empty → remove the finalizer (optimistic lock, conflict → requeue).

**IOLimiterReconciler** (key = IOLimiter; watches IOLimiter, Pod → limiters in
the namespace, PodIOLimit → limiters in `sources`): select pods (scheduled,
non-terminal, ≥1 listed volume present) → `matchedPods`. Per pod: `issues` from
`desired.Compute` plus the `PodIOLimit` volume states for volumes whose
`sources` include this limiter → applied / failed / pending (D20). Set `Ready`
and `observedGeneration`. `Status().Update`, conflict → requeue.

### 6.3 Logging & events (D26)

**Key vocabulary** (literal strings at call sites, no constants file, per logcheck):

| Key | Meaning |
|---|---|
| `pod` | `<ns>/<name>` of the target pod (on every agent and PodReconciler line) |
| `podUID` | pod UID |
| `iolimiter` | `<ns>/<name>` of an IOLimiter (on every IOLimiterReconciler line; `sources` lists them on merge lines) |
| `podiolimit` | `<ns>/<name>` of the PodIOLimit |
| `node` | node name |
| `volume` | pod volume name |
| `kubeletDir` | `kubeletDirName` (PV name or volume name) |
| `mountPoint` | host mount point matched in mountinfo |
| `mountDevice` | `MAJ:MIN` from mountinfo |
| `device` | whole-disk `MAJ:MIN` actually throttled |
| `partitionOf` | set when `mountDevice` was a partition (the mapping) |
| `cgroup` | pod cgroup path relative to the cgroup root |
| `path` | full `io.max` path written |
| `before` / `after` | the device's `io.max` line before the write and on read-back (`<none>` = no line = max) |
| `rule` | the four-key rule written |
| `op` | `apply` / `reset` / `release` |
| `state` / `from` / `to` | volume/device state, or a transition's endpoints |
| `reason` | why (the §4.2 reason vocabulary, or a skip reason) |
| `generation` / `observedGeneration` | spec generation handled |
| `finalizer` | finalizer name on add/remove |
| `elapsed` | time since first seen `Pending` |

**Agent, Info:** startup facts (node, cgroup root, discovered kubepods layout + driver, kubelet root dir, root/kubelet-dir devices, `--allow-root-device`). Every device resolution as one line: `pod volume kubeletDir mountPoint mountDevice [partitionOf] device`. Every skip with its reason (`NoBlockDevice` incl. the major-0 `mountDevice`, `VolumeNotMounted` after it first becomes a problem, `SharedRootDevice`, `CgroupNotFound`, `IOControllerDisabled`). Every intent record (D8 step 7). Every kernel write/reset: `op path device before rule after`. Read-back mismatch, drift correction (`before` = the drifted line), release start/end, status transitions (`from`/`to` per volume, `Ready` reason). The agent resolves the device from mountinfo, not `stat()` (D6), so there is no `st_dev` key: `mountDevice` is that value.
**Agent, V(1):** requeue while `Pending` (with `elapsed`), resync with no drift, "status unchanged, not written", ignored object of another node, conflict → requeue.

**Controller, Info:** `PodIOLimit` create/patch (with a diff summary: volumes added/removed/limits changed, `sources`)/delete, finalizer add/remove (and which D9 path: agent released vs pod gone/terminal), stale-UID cleanup, IOLimiter status transitions (`Ready` from/to, counts), controller-side issues when they first appear or change (volume missing, unsupported type, PVC unbound).
**Controller, V(1):** waits on a Terminating `PodIOLimit`, unbound PVC waits, no-op reconciles, status-write conflicts.

**Events** (reason, type):
- On the **Pod** (agent): `IOLimitApplied` (Normal, message lists `volume→device rule`), `IOLimitUpdated` (Normal), `IOLimitReset` (Normal, on release or volume removal), `IOLimitFailed` (Warning: `KernelRejected`/`KernelMismatch`/`IOControllerDisabled`), `IOLimitSkipped` (Warning: `NoBlockDevice`/`SharedRootDevice`), `IOLimitDriftCorrected` (Warning). Emitted only on a state transition, never per resync.
- On the **IOLimiter** (controller): `PodIOLimitCreated`/`Deleted` (Normal, names the pod), `UnsupportedVolumeType` / `VolumeNotFound` / `PVCNotBound` (Warning, on first appearance), `Ready` / `NotReady` transitions.

**Level flag:** `--zap-log-level` (debug/info/error/int) on both `/manager` and `/manager agent`. The README and skill document the kubectl patch to raise it on the Deployment and on the DaemonSet.

## 7. RBAC

| Component | Rules | Notes |
|---|---|---|
| controller (`manager-role`, ClusterRole) | `storage.maurice.fr` `iolimiters`: get,list,watch · `iolimiters/status`: get,update,patch · `podiolimits`: get,list,watch,create,patch,delete · core `pods`, `persistentvolumeclaims`: get,list,watch · `events.k8s.io` `events`: create,patch · leader-election Role (scaffold) · metrics-auth (scaffold) | No update on the main resource of any CR (D17). No nodes, no secrets. |
| agent (`agent-role`, ClusterRole) | `storage.maurice.fr` `podiolimits`: get,list,watch · `podiolimits/status`: get,update,patch · `events.k8s.io` `events`: create,patch (Pod events, D26) · bound to the scaffold's metrics-auth role (tokenreviews/subjectaccessreviews create) for secure metrics | **Nothing else.** It can list every `PodIOLimit` (RBAC can't express field selectors): contents are pod names, UIDs and limits. It cannot write any spec or metadata. |
| users | scaffolded `iolimiter-{admin,editor,viewer}` helper roles, **not** aggregated into `edit`/`admin` (OQ1 (a)). **No** helper roles for `PodIOLimit` (the `create api` for it passes `--controller=false`; delete any generated editor role). | Anyone who can create a `PodIOLimit` can throttle any pod on any node by UID. C7 adds a VAP so only the controller SA can. |

## 8. Deployment layout

- `config/` (kustomize): scaffold `default`, `crd`, `rbac`, `manager`, `prometheus`, `network-policy`, plus **`config/agent/`** (DaemonSet, ServiceAccount, `rbac/` generated role + binding, kustomization) referenced from `config/default`. The DaemonSet uses image `controller`, so the existing `kustomize edit set image controller=${IMG}` in `config/manager` covers it. Put the image transformer in `config/default` if the edit only touches `config/manager`: verify.
- `config/manager/manager.yaml` Namespace gets the PSA `privileged` labels (D14).
- Agent DaemonSet: `args: ["agent", "--health-probe-bind-address=:8081", "--metrics-bind-address=:8443"]`, env `NODE_NAME` from `spec.nodeName`, liveness `/healthz`, readiness `/readyz`, requests 10m/32Mi, limits 200m/128Mi, `updateStrategy.rollingUpdate.maxUnavailable: 10%`, securityContext + volumes per D14.
- Helm: `make helm-chart` (kubebuilder `helm/v2-alpha`, `--force`). **Known gap:** the plugin only templatizes the manager Deployment (see the reference `dist/chart/values.yaml`). The agent DaemonSet would ship with its image baked in and wouldn't follow `manager.image`/`appVersion`. C6 checks this. If it's confirmed, `hack/helm-postprocess.sh` (run by `make helm-chart`, idempotent) rewrites the agent image to `{{ .Values.manager.image.repository }}:{{ .Values.manager.image.tag | default .Chart.AppVersion }}` and adds `agent.{resources,tolerations,nodeSelector,securityContext}` values.
- Metrics: both components use the scaffold's secure metrics (8443, authn/authz filter). Probes on 8081.

## 9. Test strategy

- **Unit** (testify, `t.TempDir()` fixtures): `iomax` (format/parse/one-write-per-device via a recording writer, merge-min), `cgroup` (root discovery + path for 4 layouts × 3 QoS, refusal outside the root), `mountinfo` (octal escapes, last-entry-wins, `/mount` suffix, CSI vs local-volume dirs, major 0), `blockdev` (partition → parent through a real symlinked fake sysfs), `desired.Compute` (overlap min, unsupported types, ephemeral naming, unbound PVC), agent reconcile (fake client + fake fs: intent-before-write ordering, conflict ⇒ no kernel write, reset only owned, release).
- **envtest** (`make test`): CEL accept/reject tables for both CRDs (including the Quantity int form), `PodIOLimit` immutability, field-selector list on `spec.nodeName`, controller create/patch/delete/finalizer flows, optimistic-lock finalizer removal regression, IOLimiter status. envtest has **no GC, scheduler or kubelet**: create pods with `spec.nodeName` set, set `status.phase` via `Status().Update`, and exercise "pod gone" by deleting the pod explicitly (then assert the controller releases the finalizer, don't wait for GC). The fake client doesn't run CEL or RBAC, so CEL is envtest-only.
- **e2e** (`-tags=e2e`, pinned kind, §10): real kernel, real cgroups, real PVCs on loop devices. It asserts the node's `io.max` via `docker exec <worker> cat /sys/fs/cgroup/<status.cgroupPath>/io.max`. It **can't** be read from inside the pod: the container's cgroupns shows its own container cgroup, not the pod's. Throughput is measured with `dd … oflag=direct` / `iflag=direct` (busybox, parse dd's own summary). Every throughput test first measures an unthrottled **baseline** and fails loudly if baseline < 3× the limit ("can't prove throttling on this runner"). Throttled assertion: elapsed ≥ 0.8 × bytes/limit. Throughput tests run serially; the rest may run in parallel.

## 10. Kind & e2e fixtures

- `hack/kind-config.yaml`: 1 control-plane + 1 worker, `kindest/node:v1.36.1@sha256:3489c7674813ba5d8b1a9977baea8a6e553784dab7b84759d1014dbd78f7ebd5` (same pin as the reference). `.tool-versions`: kind 0.32.0, helm 3.21.3.
- Makefile: `KIND_CLUSTER ?= blkio-limiter`, `KCTL := kubectl --context kind-$(KIND_CLUSTER)`, `HELM := helm --kube-context kind-$(KIND_CLUSTER)`. Every kubectl/helm call is pinned.
- `hack/setup-loopdev.sh` (from `scripts/setup-loopdev.sh`), run by `make kind-setup` via `docker exec $(KIND_CLUSTER)-worker`, **idempotent**: two 512Mi files `/var/lib/blkio-e2e/disk{0,1}.img` → `losetup` → `mkfs.ext4` → mount `/mnt/blkio-e2e/disk{0,1}`. It prints `MAJ:MIN` and skips anything that already exists.
- `hack/e2e-storage.yaml`: StorageClass `blkio-local` (`kubernetes.io/no-provisioner`, `WaitForFirstConsumer`). Each test creates its own directory (`docker exec … mkdir -p /mnt/blkio-e2e/disk0/<test>`), a static **local PV** on that path (nodeAffinity = worker) and a PVC pre-bound via `volumeName`. So the pod gets a real PVC → kubelet `kubernetes.io~local-volume/<pv>` mount → loop `7:N`.
- `busybox:1.37` is pulled and `kind load`ed during `e2e-setup` (no registry pulls during the tests).
- Targets: `kind-create`, `kind-delete`, `kind-setup` (create + loopdev + storage), `kind-reset`, `kind-load`, `deploy-kind` (build, load, deploy, restart **both** the Deployment and the DaemonSet, wait for both rollouts), `e2e-setup`, `e2e` (`E2E_TIMEOUT ?= 30m`).

## 11. Chunks & progress

Every chunk ends with `make test` green (and `make lint` green from C0 onwards).
Every chunk's acceptance includes its **Logging** line: the §6.3 lines for the
code it touches exist at the stated level with the stated keys (asserted in
unit tests with a `funcr`/testr-style capturing logger where noted), and
nothing it adds logs unchanged state at Info.
Every chunk updates this file's checkboxes and, when it changes the workflow,
`.claude/skills/working-here/SKILL.md`. Git in subagents: read-only.

| ID | Title | Deps | Status |
|----|-------|------|--------|
| C0 | Scaffold, API types, CEL, repo conventions | – | Done |
| C1 | Node primitives: iomax / cgroup / mountinfo / blockdev | C0 | Done |
| C2 | Agent reconciler + `agent` subcommand + `config/agent` | C1 | Done |
| C3 | Kind harness + agent-only e2e (hand-made PodIOLimit) + CI | C2 | Done |
| C4 | Controller: PodReconciler, finalizer handshake, IOLimiter status | C0 (C2 for the finalizer contract) | Done |
| C5 | Full e2e via IOLimiter | C3, C4 | Planned |
| C6 | Helm chart, README, samples | C5 | Done |
| C7 | Hardening: VAPs + security review | C5 | Done (security-reviewer pass: user-triggered, pending) |
| C8 | Legacy removal | C6 | Done |

### C0 — Scaffold, API types, CEL, repo conventions
**Scope.** Turn the repo into a kubebuilder v4 project in place (D22) and land both API types.
- [ ] Remove the legacy `Makefile`, `Dockerfile`, `go.mod`, `go.sum` (they're in git). `kubebuilder init --domain maurice.fr --repo github.com/thomas-maurice/k8s-blkio-limiter --project-name k8s-blkio-limiter`. If init refuses the non-empty dir, scaffold in a scratch dir and copy over. **Keep** `cmd/k8s-blkio-limiter`, `internal/limiter`, `charts/`, `dev/`, `scripts/`, `.github/workflows/*` compiling and untouched.
- [ ] `kubebuilder create api --group storage --version v1alpha1 --kind IOLimiter --resource --controller`. `kubebuilder create api --group storage --version v1alpha1 --kind PodIOLimit --resource --controller=false`. `kubebuilder create api --group core --version v1 --kind Pod --controller --resource=false` (the PodReconciler shell). Leave the reconcilers as no-op stubs.
- [ ] Types + markers exactly as §4 (adjust only if CEL won't compile, and record why here).
- [ ] Replace ginkgo scaffolding with testify (the envtest suite as in the reference `internal/controller/envtest_test.go`). Copy `.golangci.yml`, `.custom-gcl.yml`, `.tool-versions`, `hack/boilerplate.go.txt` conventions and `AGENTS.md` from the reference, and write `.claude/skills/working-here/SKILL.md` adapted to this repo.
- [ ] Makefile: the reference's `test` (with `CGO_ENABLED=0`), `lint`, `verify`, `build`, pinned `KCTL`/`HELM`. No cert-manager target (D13).

**Files.** `PROJECT`, `Makefile`, `Dockerfile`, `go.mod`, `cmd/main.go`, `api/v1alpha1/*`, `config/**`, `internal/controller/*` (stubs + envtest), `hack/`, `AGENTS.md`, `.claude/skills/working-here/SKILL.md`, lint configs.

**Acceptance.**
- `make manifests generate` is clean and idempotent (second run, no diff).
- `make test` green, including: an envtest CEL table where valid `IOLimiter`s (Quantity as `10Mi` **and** as integer `1048576`) are accepted, and these are rejected: `100m`, `512` (below 1Ki), no limits set, IOPS 1, IOPS 2^32, duplicate volume names, `PodIOLimit` spec identity mutation. Also a field-selector `List` on `spec.nodeName` returns only matching `PodIOLimit`s.
- `make lint` green. The legacy `go test ./internal/limiter/...` still passes under the new go.mod.
- **Logging:** `cmd/main.go` zap options identical to the reference (`Development: true`, `Level: info`, `BindFlags`). `.custom-gcl.yml` logcheck active. The skill file has the §6.3 key table and levels.

**Test plan.** envtest only (no logic yet). **Out of scope.** Any reconcile logic, agent, deployment manifests beyond the scaffold.

**Deviation from §4 (found in C0, proven by envtest, not guessed).** The
1Ki..1Pi whole-number-bytes CEL rule on `IOLimits.readBytesPerSecond` /
`writeBytesPerSecond` does not compile *in the budget sense*: the apiserver
rejects the CRD at install time with "estimated rule cost exceeds budget",
even for a **single**, comparison-free `quantity(string(self))` or
`isQuantity(string(self))` call (measured ~1.006x over budget for one call,
~2x for two, given `volumes[]`'s spec-mandated `MaxItems=16`). There is no
controller-gen marker to bound the field's `maxLength` — it rejects
`MaxLength`/`Pattern` on a `resource.Quantity` Go field ("must apply
maxlength to a textual value") — so the CEL cost estimator always assumes
an unbounded string and the rule can never fit, regardless of which
Quantity-library function is used. §4's own anticipated fallback
(`isQuantity(string(self))`) was tried first and hits the same wall.
Resolution: both fields keep only the schema's automatic `anyOf`
int/string + pattern (still reject garbage strings) and the existing
`IOLimits`-level `has(...)` "at least one limit set" rule (cheap, no
`quantity()` call, passes). The 1Ki..1Pi range and whole-number check move
to a controller-side check (desired.Compute in C4, or a C7
`ValidatingAdmissionPolicy` — there's no admission webhook per D13 to do it
earlier). `internal/controller/envtest_test.go`
(`TestIOLimiter_CEL_QuantityLimits_Envtest`) documents the resulting
behaviour explicitly (`100m` and `512` are currently **accepted**) so a
future fix is a visible test change, not a silent regression.

### C1 — Node primitives
**Scope.** Pure, host-path-injectable packages. No Kubernetes imports except `types.UID` / `corev1.PodQOSClass`.
- [ ] `internal/iomax`: `Rule{ReadBPS,WriteBPS,ReadIOPS,WriteIOPS *uint64}`, `Format` (always four keys, `max`), `Parse(io.max content) map[dev]Rule`, `Write(path, dev, rule)` (**one `write()` per call**), `Reset(path, dev)`, `Merge(a, b) Rule` (per-field min, nil = max). Reset treats `ENOENT`/`ENODEV` as success.
- [ ] `internal/cgroup`: `DiscoverKubepods(root, override) (Layout, error)` over the §D6 candidates, `(Layout).PodPath(uid, qos) (string, error)` for systemd/cgroupfs (K9), refusing any result outside the kubepods root. `IOControllerEnabled(path)`.
- [ ] `internal/mountinfo`: `Parse(r)` (fields 3 and 5, `\040`-style octal unescape), `FindKubeletVolume(entries, kubeletRoot, podUID, dirName) (maj, min, error)` with last-match-wins and the optional `/mount` suffix. Rewritten from `findBlockDeviceFromMountinfo`.
- [ ] `internal/blockdev`: `WholeDisk(sysRoot, maj, min) (maj, min, isPartition, error)` via the `partition` file + `EvalSymlinks` (K4). `IsRootDevice(...)` for D23 (device of `/` and of the kubelet root dir, from the same mountinfo).

**Acceptance.** Unit tests cover: every layout × QoS (incl. kind's `kubelet.slice/kubelet-kubepods.slice`); a UID with dashes → underscores for systemd only; mountinfo with CSI `/mount`, local-volume, shadowed mount, escaped path, major 0; a fake sysfs where `sda1 → ../sda` resolves to `8:0` and a lexical join would give the wrong answer (the test must fail on a lexical implementation); `Write` issues exactly one `write()` per device (recording file); `Format` of an all-nil rule equals the reset line. `make test` green.

**Logging:** the primitives stay log-free. They return structured results (`Resolution{MountPoint, MountDevice, PartitionOf, Device, SkipReason}`, `WriteResult{Before, After}`) so the agent can log §6.3 lines without re-reading anything. The unit tests assert those fields are populated, including `Before`/`After` on writes and resets.

**Test plan.** Unit only: pure logic with fs fixtures. The real-kernel behaviour (K1, K2, K6) is proven in C3. **Out of scope.** Controllers, the agent loop, deleting `internal/limiter`.

**Deviation from the chunk description (found in C1 review, not guessed).**
`mountinfo.FindKubeletVolume` returns the matched `mountinfo.Entry` (mount
point + device), not a bare `(maj, min, error)`: C2 needs the mount point to
log it (§6.3 `mountPoint`) without re-reading mountinfo. `mountinfo.ParseDevice`
is exported (was an unexported `parseDevice` duplicated in both `mountinfo`
and `blockdev`) so `blockdev` parses `Entry.Device`/sysfs `dev` file content
through the one function. `blockdev.WholeDisk` does not separately return the
partition's own `maj:min`: it's exactly the `(maj, min)` the caller passed
in, so there's nothing to add. The `Resolution` struct named in this chunk's
Logging paragraph is not a C1 type: it's assembled in `internal/agent` (C2)
from `mountinfo.Entry` + `blockdev.WholeDisk`'s return values plus the
agent's own skip-reason logic.

### C2 — Agent reconciler + subcommand + manifests
**Scope.** §6.1 end to end, minus e2e.
- [x] `internal/agent`: `Main(ctx, args) int` (own `flag.FlagSet` + zap flags), a controller-runtime manager with the node-scoped cache (D16), no leader election, `PodIOLimitReconciler` implementing §6.1, metrics, `readyz` gated on discovery. RBAC markers → `agent-role` (D15).
- [x] `cmd/main.go`: `if len(os.Args) > 1 && os.Args[1] == "agent" { os.Exit(agent.Main(ctrl.SetupSignalHandler(), os.Args[2:])) }` before manager flag parsing.
- [x] `config/agent/` DaemonSet/SA/RBAC per §8 + D14, wired into `config/default`. Namespace PSA labels.
- [x] Makefile `manifests`: two controller-gen RBAC passes (D15).

**Acceptance.**
- Unit (fake client + temp-dir fs): new `PodIOLimit` → status `Pending` written **before** the fake `io.max` changes. A status conflict → `io.max` untouched. Two volumes on two devices → two separate writes. Two volumes on one device → one min-merged rule and both statuses name it. A volume removed from the spec → only that device reset. A device present in `io.max` but absent from `status.devices` is never touched. Deletion → all owned devices reset, then `devices: []` + `Released`. Pod cgroup missing on deletion → `Released` without error. Drift (an external write to fake `io.max`) → rewritten on resync, metric incremented. `spec.nodeName` of another node → ignored. Two volumes on one device → both `Applied/SharedDevice`. Root-disk volume → `Unsupported/SharedRootDevice`, and applied with `--allow-root-device`. Liveness (D24): fails with an injected fake clock once no reconcile completes for 3 periods while objects exist, and stays healthy with zero objects and with a permanently `Failed` volume.
- **Logging/events:** a capturing logger in the unit tests asserts, at Info with `pod=<ns>/<name>`: the startup facts line, one resolution line per volume (with `partitionOf` in the partition fixture), a skip line with `reason` for major 0 / root device, an intent line, `op=apply path device before rule after` per write, `op=reset` per stale device, and a drift line. Asserts nothing is logged at Info on a no-drift resync. A fake recorder asserts `IOLimitApplied`/`IOLimitFailed`/`IOLimitSkipped`/`IOLimitReset`/`IOLimitDriftCorrected` on the Pod reference, once per transition. `--zap-log-level` is accepted by `manager agent`.
- `kustomize build config/default` renders the DaemonSet with **no** `privileged`, **no** `hostPID`, `capabilities.drop: [ALL]`, and the two hostPath mounts from D14. `agent-role` contains exactly the §7 rules.
- `make test`, `make lint`, `make docker-build` green.

**Test plan.** Unit (fake client, fs fixtures). E2E deferred to C3 (same codebase, needs the harness). **Out of scope.** Controller logic, helm.

**Deviation/implementation notes (found in C2, not guessed).**
- **Plain-file `io.max` fixtures can't hold two different devices'
  written lines simultaneously.** `iomax.Write`/`Reset` (C1, unchanged)
  issue one bare `O_WRONLY` `write()` per call with no `O_TRUNC`/`O_APPEND`,
  matching the real kernel's per-device-state cgroup file (a `write()`
  there updates one device's kernel-held rule; it never touches "file
  bytes"). A plain OS file used as a test double has no such per-device
  state: a second `write()` to a *different* device just overwrites bytes
  at the same offset, corrupting whatever a prior write left there. This
  is exactly what C1's own test plan anticipates ("The real-kernel
  behavior (K1, K2, K6) is proven in C3"). The C2 unit tests that exercise
  two distinct devices in one pod cgroup
  (`TestReconcile_TwoVolumesTwoDevices_TwoWrites`,
  `TestReconcile_VolumeRemoved_OnlyThatDeviceReset`) therefore assert
  correctness via each write's own captured log line (`before`/`after`
  come from `iomax.Write`'s own read-back, taken synchronously at the
  moment of that write, so they're accurate regardless of what a later
  write does to the file) and via `status.devices`/`status.volumes`, not
  by re-reading the file for multiple devices at once. Real multi-device
  `io.max` content is proven by C3's real-kernel e2e
  (`TestAgentTwoDevices`).
- **Agent startup preflight (cgroup2 present, kubepods root discovered,
  `io` in `cgroup.subtree_control`, proc-root mountinfo readable) fails
  fast (`os.Exit`-equivalent, return code 1) rather than being retried
  behind a permanently-red `readyz`.** These are static host-mount facts
  that won't become true without a pod restart, so a kubelet
  crash-loop-backoff restart is the recovery path (D24's own reasoning: a
  restart is harmless, D8 has no startup wipe). `readyz`/`healthz` are
  still gated on a periodic self-check loop (`runSelfCheckLoop`, every
  `--resync-period`) and on cache sync, per D24 -- only the *very first*
  pass is fail-fast rather than retried in place.
- **`IOControllerDisabled` (io controller missing on an otherwise-present
  pod cgroup) requeues at `--resync-period`, not the 2s `Pending` pace.**
  §6.1 only specifies the 2s pace for "cgroup/mount not there yet"
  (a `Pending` condition expected to resolve soon); a disabled io
  controller is a persistent misconfiguration (`Failed`), so polling it
  every 2s forever would be pure overhead with no expectation of a
  near-term fix.
- **A `PodIOLimit` whose every volume is `Unsupported` reports
  `Ready: False/Unsupported`** (coordinator decision, overriding this
  chunk's original draft, which folded it into `True/Applied` as "settled,
  non-blocking" -- rejected as misleading: nothing was actually applied).
  §4.2's documented reason set (`Applied | Pending | Failed | Released`) is
  extended with a fifth reason, `Unsupported`, on the `Conditions[type=Ready]`
  entry only (`PodIOLimitStatus.Conditions` is a plain `[]metav1.Condition`
  with no CRD `enum:` constraint on `reason` -- confirmed against the
  rendered CRD schema -- so this is a doc-comment change in
  `api/v1alpha1/podiolimit_types.go` plus `make manifests generate`, not an
  API-breaking schema change). A volume that's `Unsupported` *alongside* at
  least one `Applied` volume still reports `True/Applied`: the agent did
  everything it could for the volumes it could act on, and only an
  all-`Unsupported` `PodIOLimit` is misleading as "Applied".
  `internal/controller/iolimiter_controller.go`'s `IOLimiterReconciler`
  needed no change: it already treats a volume's own
  `VolumeStatus.State == "Unsupported"` as failed for its aggregation
  (`podIOLimitStateFor`), and never reads the agent's PodIOLimit-level
  `Ready` condition at all -- confirmed by grep, the two are independent.
  `TestReadyReason` (table test) and
  `TestReconcile_RootDevice_UnsupportedUnlessAllowed`'s new assertion cover
  this.
- **D7's SharedDevice message names the *other* volumes sharing the
  device, not all of them including the volume itself** (`resolve.go`'s
  `otherVolumes`), matching §4.2's literal wording ("message naming the
  other volumes and the effective rule").
- **The agent's own image-transform gap, flagged in §8 as "verify":**
  `cd config/manager && kustomize edit set image controller=${IMG}` only
  rewrites resources kustomize builds *from* `config/manager` (the
  Deployment); `config/agent`'s DaemonSet is a sibling base under
  `config/default` and was silently left on `controller:latest`. Fixed by
  moving the `edit set image` calls in the Makefile's `deploy` and
  `build-installer` targets from `config/manager` to `config/default`
  (verified: both the Deployment and the DaemonSet pick up `${IMG}` after
  the change, neither did with only the `config/manager` edit).
- **D14's namespace PSA labels are added via a JSON6902 patch in
  `config/default/namespace_psa_patch.yaml`**, not by editing
  `config/manager/manager.yaml` (which defines the shared `Namespace`
  object): keeps the controller's own manifest agent-agnostic and the
  change inside this chunk's `config/agent` + "wiring into config/default"
  remit.
- **`internal/agent`'s own sysfs root (`/sys`, D14's "own read-only
  sysfs") is a package constant, not a flag**: §6.1's flag list doesn't
  include a `--sys-root` override, and there's no host bind mount to
  parameterize (the container's own `/sys/dev/block` is used as-is).

**Coordinator review fixes (post-implementation, C2).**
- **D24(a) was dead**: `recordSelfCheck` updated the timestamp on both
  success and failure, and `livezCheck` never read `selfCheckOK` at all, so
  a self-check that kept running but kept *failing* looked live forever.
  Fixed: `livezCheck` now also fails when the latest recorded self-check
  was a failure, not just when it's stale.
- **`IOLimitFailed` fired on every resync** while a device stayed
  KernelRejected/KernelMismatch, instead of once on the transition into
  that state. Fixed via a per-device "is this the same failure as last
  time" check (`failureIsRepeat`, comparing every affected volume's
  previous `(state, reason)` against the new one) -- the log line
  (`"Kernel write failed"`/`"Kernel write"`) still fires every resync (D11:
  retries are real, observable activity), only the event is gated.
  `iomax.Write` is now called through a package-var seam (`ioMaxWrite`,
  mirroring `iomax`'s own `openForWrite` pattern) so tests can force
  KernelRejected/KernelMismatch without a real kernel.
- **`IOLimitApplied`/`IOLimitUpdated` messages now name the volume(s)**
  (`<volumes>→<device>: <rule>`), matching §6.3's "message lists
  `volume→device rule`" literally -- they previously said only
  `device %s: %s`.
- Added `TestReconcile_LogLines_CarryPodKey`, asserting `pod=<ns>/<name>`
  on the resolution, intent, `op=apply`, `op=reset` and drift-corrected
  Info lines specifically (D26).
- **`release()` with zero owned devices now still writes
  `Ready=False/Released`** before returning, instead of leaving status
  untouched -- consistent with D9 for a `PodIOLimit` deleted before its
  first successful apply.

### C3 — Kind harness + agent-only e2e + CI
**Scope.** Prove the risky parts (K1, K2, K6, D14 non-privileged writes, D6 kind layout) on a real kernel **before** building the controller. The test plays the controller: it creates `PodIOLimit`s by hand (with the finalizer) and removes the finalizer itself.
- [x] `hack/kind-config.yaml`, `hack/setup-loopdev.sh`, `hack/e2e-storage.yaml`, Makefile kind/e2e targets (§10).
- [x] `test/e2e/harness_test.go` (build tag `e2e`, pinned context, `TestMain` preflight: DaemonSet ready on the worker, loop mounts present; self-healing cleanup of leftover test namespaces), builders for local PV/PVC/pod, `nodeIOMax(t, cgroupPath)` via `docker exec`, `ddThroughput(t, pod, args)`.
- [x] Tests: **TestAgentAppliesAndThrottles** (write limit 5Mi + wiops; `io.max` line exact incl. `rbps=max`; baseline ≥ 3× limit, throttled ≥ 0.8 × expected duration), **TestAgentPartialUpdateResetsOtherKeys** (K2: riops set, then only wbps → `riops=max` on the node), **TestAgentTwoDevices** (disk0 + disk1 → two lines, regression for the legacy K1 bug), **TestAgentReleaseResetsOnlyOwned** (a manually written rule on another device in the same pod cgroup survives the release), **TestAgentSurvivesContainerRestart** (stop the container, rule still there, no status churn), **TestAgentRestartKeepsRules** (rollout restart of the DS → rule never absent across the restart, sampled every 200ms). Plus **TestAgentDaemonSetIsNotPrivileged** (D14 asserted directly on the live DaemonSet spec).
- [x] `.github/workflows/ci.yml` modelled on the reference: unit (lint/test/build) → e2e (`make e2e-setup e2e`, diagnostics on failure: agent + controller logs, `kubectl get podiolimits -A -o yaml`, node `io.max` dump) → publish image on master/tags. Legacy workflows left alone (C8).

**Acceptance.** `make e2e-setup && make e2e` green twice in a row on a fresh `make kind-reset` (verified locally, twice, plus two more consecutive `make e2e` runs without a reset in between: six green runs total). The agent pod spec is asserted non-privileged in the test (`TestAgentDaemonSetIsNotPrivileged`). **Logging/events:** on failure the diagnostics step (harness `dumpDiagnostics` and the CI workflow) dumps the agent DaemonSet's logs (both pods, `-l control-plane=agent`) grepped for the test namespace, plus `podiolimits -o yaml`, events and the node's `io.max`. `TestAgentAppliesAndThrottles` asserts the agent log for its pod contains the resolution line (with loop `7:0`) and the `op=apply` line, and that the Pod has an `IOLimitApplied` event -- verified against the real agent JSON log output on kind. D14 did **not** fail on kind: the agent (uid 0, no capabilities, no hostPID, `readOnlyRootFilesystem: true`) wrote and reset `io.max` successfully in every test; no K-fact needed correcting in §2.

**Test plan.** e2e as listed; unit unchanged. **Out of scope.** Controller-driven flows (C5).

**Deviation/implementation notes (found in C3, not guessed).**
- **`kind load docker-image` for busybox:1.37 is unreliable on this host**
  (Docker Desktop for Mac, containerd image store): `docker save | docker
  exec … ctr images import --all-platforms` fails with `content digest
  … not found` even for a single-platform local pull, because the saved
  archive's multi-platform manifest list references other-platform blobs
  the local store never fetched. `make kind-load-busybox` instead runs
  `ctr --namespace=k8s.io images pull docker.io/library/busybox:1.37`
  directly inside each node's own containerd (nodes have outbound network
  in both this environment and CI), which sidesteps the archive
  round-trip entirely and is what `.github/workflows/ci.yml` also uses.
  Still happens once during `make kind-setup`/`e2e-setup`, never during a
  test (§10's "no registry pulls during the tests" is preserved).
- **C4 (controller) landed before this chunk ran against a live cluster**,
  even though C3 is specced to run "before building the controller"
  (§11's own framing). `make deploy-kind` deploys the real
  controller-manager Deployment alongside the agent DaemonSet in the same
  namespace (D4/`config/default`): left running, its `PodReconciler`
  correctly (per §6.2 step 4) treats any `PodIOLimit` with no matching
  `IOLimiter` as an orphan and deletes it -- which includes every
  hand-crafted `PodIOLimit` this suite creates. Observed directly: a
  `TestAgentSurvivesContainerRestart` run got its object deleted out from
  under it (`IOLimitReset "all rules released"` event, then
  `podiolimits.storage.maurice.fr "…" not found`) within seconds of the
  container-restart-triggered Pod reconcile. This is correct controller
  behaviour, not an agent bug. Fix, confined to the test harness
  (`test/e2e/harness_test.go`, `scaleControllerManagerToZero`): `TestMain`
  scales `k8s-blkio-limiter-controller-manager` to 0 replicas before
  running any test and leaves it there for the suite's lifetime;
  `make deploy-kind` (`replicas: 1` in `config/manager/manager.yaml`)
  restores it on the next deploy. C5 is the chunk that needs the
  controller running again for its own tests.
- **"kill the container" (chunk description) does not work against this
  image via `kubectl exec … kill -9 1`.** Verified interactively: sending
  SIGKILL to a busybox `sleep infinity` pid 1 (via `kill -9 1` executed in
  the same pid namespace) leaves the process completely unaffected --
  same pid, same containerID, same `startedAt`, a second later and after
  retrying. `crictl stop <containerID>` on the node (containerID read
  from `status.containerStatuses[0].containerID`) reliably stops the
  container and lets kubelet's `restartPolicy: Always` recreate it
  (`restartCount` increments, new containerID), which is what
  `TestAgentSurvivesContainerRestart` actually needs: a container restart
  with the pod (and its cgroup) untouched.
- **`kubectl logs ds/<name>` silently picks exactly one of the two
  matching agent pods** (control-plane + worker) when given a DaemonSet
  selector, and may pick the control-plane one, which never reconciles
  this suite's PodIOLimits (all test pods are pinned to the worker). Both
  `dumpDiagnostics` and `TestAgentAppliesAndThrottles`'s own log
  assertion use `kubectl logs -n … -l control-plane=agent --prefix`
  instead, which streams every matching pod.
- **PV names are derived from the (already-random) test namespace name**,
  not fixed per-test literals: a fixed PV name collides with a PV left
  behind by an earlier failed run of the same test (PVs are cluster-scoped
  and this suite's cleanup only skips namespace/PodIOLimit deletion on
  `t.Failed()`, not the PV's), producing `object is being deleted:
  persistentvolumes "…" already exists` on the next run.

### C4 — Controller
**Scope.** §6.2.
- [x] `internal/controller/desired` (pure `Compute`), `PodReconciler`, `IOLimiterReconciler`, field indexes, cache transform (D16), RBAC markers → `manager-role`.
- [x] `cmd/main.go` wiring. No webhooks (`ENABLE_WEBHOOKS` irrelevant; don't scaffold any).

**Acceptance.**
- Unit on `Compute`: two overlapping limiters → per-field min with sorted sources; volume absent → not in the output and not an issue; `emptyDir`/`hostPath`/block-mode PVC → `UnsupportedVolumeType` issue; generic ephemeral → claim `<pod>-<vol>`; unbound PVC → `PVCNotBound` issue.
- envtest: IOLimiter + scheduled pod + bound PVC → `PodIOLimit` with the D18 name, ownerRef, finalizer, merged limits, `kubeletDirName` = PV name. Edit the limiter → spec patched (generation bumps), and no full Update is observed (use an envtest CEL guard or assert the resourceVersion path). Delete the limiter → `PodIOLimit` gets a deletionTimestamp and **keeps** the finalizer while `status.devices` is non-empty. Set status `devices: []` → finalizer released. Pod deleted → finalizer released without any status. Same-name pod with a new UID → the old `PodIOLimit` is released and a new one created. Stale-cache finalizer removal conflicts instead of succeeding (regression test for D9). IOLimiter status: matched/applied/failed counts and `Ready` reasons for each D20 case, `observedGeneration` set.
- **Logging/events:** a capturing logger asserts Info lines (with `pod=` or `iolimiter=`) for create/patch (diff summary)/delete, finalizer add/remove naming the D9 path, stale-UID cleanup, `Ready` transitions. Conflicts only at V(1). A replayed identical reconcile logs nothing at Info. The recorder asserts the §6.3 IOLimiter events, once per transition. Transition logs/events are only emitted after a successful status write (test: a forced conflict emits none).
- `make test`, `make lint` green.

**Test plan.** Unit + envtest (see §9 envtest caveats). **Out of scope.** e2e (C5), extra metrics.

**Deviation/implementation notes (found in C4).**
- **`desired.Compute` enforces the C0-deferred bps range check.** Per the
  C0 deviation note, CEL can't afford the 1Ki..1Pi whole-number-of-bytes
  check on `readBytesPerSecond`/`writeBytesPerSecond`, so `Compute`
  validates it controller-side (`validateBPS`, mirroring CEL's
  `quantity(string(self)).isInteger()` via `Quantity.Value()` vs the
  original, plus the 1Ki/1Pi bounds). A field that fails is reported as
  issue reason `InvalidLimit` and dropped from that limiter's contribution
  to the merge (D2's per-field minimum simply never sees it) -- it does
  **not** invalidate the limiter's other, valid fields, and does not
  block other limiters' valid contributions to the same volume. If a
  volume ends up with zero valid fields from every contributing limiter
  (e.g. a limiter whose only field was invalid), the volume is dropped
  from the `PodIOLimit` output exactly as if no limiter had named it at
  all -- the `InvalidLimit` issue is still recorded, so the problem isn't
  silently lost, it just contributes no kernel rule. Unit tests
  `TestCompute_InvalidLimit_100m_512` (both `"100m"` and `"512"`, per the
  C0 envtest's own accept table) and
  `TestCompute_InvalidLimit_DoesNotBlockOtherValidFields` cover this.
  IOPS fields are **not** re-validated here: the CRD schema's
  `Minimum=2`/`Maximum=4294967295` markers already enforce that range at
  admission (proven by `TestIOLimiter_CEL_QuantityLimits_Envtest`), so a
  second controller-side check would be redundant.
- **"volume absent" in this chunk's acceptance bullet means a *pod*
  volume no limiter references**, not a *limiter* volume the pod doesn't
  have. The latter (a limiter names a volume absent from
  `pod.spec.volumes`) is D20's "volume missing in pod" and **is** an
  issue (`VolumeNotFound`, with the `VolumeNotFound` warning event D26
  documents) -- it would be a silent typo otherwise. `Compute` simply
  never looks at a pod volume no limiter names, so it can never appear in
  the output or as an issue; that's the case the acceptance bullet
  exercises (`TestCompute_VolumeAbsentFromPod_NotOutputNotIssue`).
  `TestCompute_VolumeNotFoundInPod` covers the D20 case separately.
- **IOLimiter warning-event "first appearance" dedup is message-based,
  not stateful.** `IOLimiterReconciler` has no extra CR field to remember
  which issues it already warned about, and deliberately doesn't keep
  in-memory reconciler state (a restart must never re-derive a wrong
  answer). It instead compares each new `"<pod>: volume <v>: <reason>:
  <msg>"` entry against the *previous* Ready condition's message (already
  on the object) via a substring check, and only emits/logs the ones not
  already present. This is exact as long as the failing-pod list stays
  within the 5-entry cap (D20); beyond that, an issue that scrolls out of
  the capped message could re-emit its warning event on a later
  reconcile. Acceptable given D20's cap is meant for human-readable
  status, not an audit log, but worth a second look if `PVCNotBound` events
  are seen to repeat on a namespace with dozens of failing pods.

### C5 — Full e2e via IOLimiter
- [x] Rewrite the C3 tests' setup to go through `IOLimiter` where they test user-facing behaviour. Keep a lean agent-only test for K2.
- [x] **TestIOLimiterEndToEnd** (StatefulSet, 1 replica, PVC template on `blkio-local`: `Ready=True`, `appliedPods=1`, `io.max` exact, throttled throughput, delete the limiter → line gone, baseline throughput back, `PodIOLimit` gone).
- [x] **TestOverlappingLimitersMostRestrictive**, **TestLimiterUpdatePropagates**, **TestUnsupportedVolumeReported** (emptyDir → `failedPods=1`, message names the pod), **TestPodRecreatedGetsNewPodIOLimit** (sts pod delete → new UID, new object, old one released).
- [x] **TestDeleteWhileAgentDown**: patch the DS `nodeSelector` to exclude the worker, delete the limiter → `PodIOLimit` stays Terminating, `io.max` untouched. Restore the DS → reset + object gone. Restore the DS in `t.Cleanup` **and** in `TestMain` self-heal.

**Acceptance.** Full suite green twice consecutively on a fresh cluster. Wall time < 15 min on CI. **Logging/events:** TestIOLimiterEndToEnd asserts that `grep <ns>/<iolimiter>` on the controller log and `grep <ns>/<pod>` on the agent log together contain the full narrative: PodIOLimit created → resolved → intent → apply → Ready → (delete) → release → reset → finalizer removed. It also asserts the `IOLimiter` and Pod events. TestUnsupportedVolumeReported asserts the `UnsupportedVolumeType` warning event. **Out of scope.** Multi-node scheduling spread, SELinux.

**Structure (decided in C5, superseding C3's single `test/e2e` package).** C3's `TestMain` scaled the controller to 0 for its whole suite because a running controller correctly deletes any hand-made `PodIOLimit` with no backing `IOLimiter` (§6.2 step 4) -- exactly what every C3 test builds. C5 needs the opposite: a live controller, driving everything through `IOLimiter`. Rather than one package juggling both, the suite is now three packages:
- `test/e2e/harness` (build tag `e2e`, plain package, not `_test.go`): every shared piece both suites need -- pinned-context clients (`Init`), DaemonSet/Deployment readiness and self-heal helpers, `ScaleControllerManager`, the `Exclude/RestoreAgentDaemonSet` pair, namespace/poll/diagnostics helpers, and the PV/PVC/Pod/StatefulSet/IOLimiter/PodIOLimit builders. Exported so both suites below import it normally.
- `test/e2e/agent`: the C3 agent-only tests, hand-crafting `PodIOLimit`s. `TestMain` scales `controller-manager` to 0 for the package's lifetime and **always** restores it to 1 in a `defer` before returning, success or failure. Kept: `TestAgentPartialUpdateResetsOtherKeys` (K2, explicitly named to stay lean-agent-only), plus the other pre-existing kernel/agent-mechanism tests (`TestAgentTwoDevices` -- K1 regression, `TestAgentReleaseResetsOnlyOwned` -- D8 ownership against a hand-injected foreign rule, `TestAgentSurvivesContainerRestart`, `TestAgentRestartKeepsRules`, `TestAgentDaemonSetIsNotPrivileged` -- D14) since none of them have an `IOLimiter`-driven equivalent that would test the same mechanism (see the deviation note below). `TestAgentAppliesAndThrottles` is dropped: `TestIOLimiterEndToEnd` is its IOLimiter-driven superset (same resolution/apply log assertions, `IOLimitApplied` event, throttle-throughput measurement).
- `test/e2e/operator`: every C5 test, all `IOLimiter`-driven, controller running throughout. `TestMain` self-heals the controller to 1 replica and the agent DaemonSet's `nodeSelector` to unset on entry (in case a previous killed run, in either package, left either mutated), and does the same again in a `defer` on exit.

`make e2e` runs `go test ./test/e2e/...` **with `-p 1`**: `go test` otherwise builds/runs each package's test binary concurrently (bounded by `GOMAXPROCS`, independent of `-count=1`), which raced `agent`'s and `operator`'s `TestMain`s against each other in practice (observed directly: `operator`'s self-heal scaling the controller back to 1 while `agent`'s tests still assumed it was 0, and `TestDeleteWhileAgentDown` evicting the agent from the worker mid-run of unrelated `agent`-package tests) -- `-p 1` forces the two packages to run strictly one after the other.

**Deviation/implementation notes (found in C5, not guessed).**
- **A real, pre-existing aliasing bug in `IOLimiterReconciler.Reconcile`** (`internal/controller/iolimiter_controller.go`, landed in C4): `oldReady := meta.FindStatusCondition(cr.Status.Conditions, "Ready")` captured a *pointer* into `cr.Status.Conditions`; `meta.SetStatusCondition` (called later in the same reconcile, via `setIOLimiterReady`) mutates an existing condition of the same `Type` **in place** rather than replacing it. So on every reconcile after the first one that finds an existing `Ready` condition, `oldReady` silently aliased the just-written new value by the time it was compared against `newReady`, and the transition (and its event) was never logged/emitted even though the Ready reason genuinely changed. Found via `TestIOLimiterEndToEnd`: a real IOLimiter that went `Pending` → `AllApplied` only ever logged `"to": "Pending"` and never emitted a `Ready` event. Fixed by snapshotting `oldReady` by value immediately after finding it. Regression test: `TestIOLimiterReconciler_ReadyTransition_SecondReconcileLogsAndEmits` (reconciles the same object twice, asserting the second reconcile's transition is logged and evented; verified it fails without the fix).
- The controller-side unsupported-volume event reason is `UnsupportedVolumeType` (`desired.ReasonUnsupportedVolume`); §6.3 was aligned to the code.
- **`go test ./test/e2e/...` parallelizes across packages by default even with `-count=1`** (see "Structure" above) -- `make e2e` now passes `-p 1`.
- **A StatefulSet pod recreation can be faster than a 2s poll interval**: waiting for the old pod name to 404 before waiting for it to reappear (mirroring C3's `WaitPodGone` pattern) flaked, because the delete-then-recreate sometimes happens entirely between two polls -- the poll only ever observes the old UID, then directly the new one, never a gap. `harness.WaitPodRecreated` polls for "same name, different (non-empty) UID, Ready" instead of requiring an intermediate NotFound.

### C6 — Helm chart, README, samples
- [x] `make helm-chart` + the §8 post-process (confirmed needed, see deviation notes). `helm lint dist/chart`. Installed the chart on kind (`helm-deploy`) in place of the kustomize deploy and ran the smoke e2e (`TestIOLimiterEndToEnd`); then restored `make deploy-kind`.
- [x] README rewrite: what/why, install (kustomize and helm), API reference table, how limits map to `io.max`, K7 caveats (shared device, rootfs), dirty-page/OOM note linking `.ideas/dirty-pages-and-oom.md`, annotation → IOLimiter migration (D3), supported volume types (D19), trust model (§7).
- [x] `config/samples/` real-world IOLimiters. CI: helm-lint + chart push on tags as in the reference.

**Acceptance.** Rendering the chart with `--set manager.image.tag=X` puts `X` on **both** the Deployment and the DaemonSet (verified: `helm template ... --set manager.image.tag=X` greps `image: "ghcr.io/thomas-maurice/k8s-blkio-limiter:X"` on both the Deployment and the DaemonSet). **Logging:** the README and skill have a "Logging & debugging" section (key table, levels, how to raise `--zap-log-level` on the Deployment and the DaemonSet, and the grep recipes). The chart exposes agent args, so the level is settable via values. Chart smoke e2e green (`TestIOLimiterEndToEnd`, `test/e2e/operator`, against the helm-installed release). **Out of scope.** New features.

**Deviation/implementation notes (found in C6, not guessed).**
- **§8's "known gap" is confirmed, and worse than documented.** The
  kubebuilder `helm/v2-alpha` plugin's generated
  `dist/chart/templates/extras/agent.yaml` doesn't just bake in the agent's
  image: every field the spec's acceptance bullet asks to expose
  (`resources`, `tolerations`, `nodeSelector`, `securityContext`,
  `priorityClassName`, args) is a hardcoded literal, not a `.Values.*`
  reference. Worse, its `serviceAccountName` references
  `{{ include "k8s-blkio-limiter.resourceName" (dict "suffix" "agent" ...) }}`
  but the plugin never emits a ServiceAccount with that name — only the
  manager's own SA is templated (`templates/rbac/controller-manager.yaml`,
  gated on `.Values.serviceAccount.enabled`) — so the unmodified generated
  chart's agent DaemonSet pods would fail to start (no such
  ServiceAccount). `hack/helm-postprocess.sh` (idempotent, run by `make
  helm-chart` after the plugin) rewrites `agent.yaml` in full (image via
  `.Values.manager.image`, so `--set manager.image.tag=X` lands on both
  workloads; args extended with `--resync-period`, `--allow-root-device`,
  `--kubepods-cgroup` and a free-form `agent.args` list for
  `--zap-log-level` etc.; resources/securityContext/podSecurityContext/
  tolerations/nodeSelector/priorityClassName all from a new `agent:` values
  block) and adds the missing
  `dist/chart/templates/rbac/agent-serviceaccount.yaml`. Verified on kind:
  the chart-installed agent DaemonSet pods start and reach `Ready` with the
  new ServiceAccount.
- **`dist/chart` is checked into git**, reversing this repo's original
  `dist/` blanket gitignore (only `dist/install.yaml`, the
  `build-installer` kustomize output, stays ignored). Matches the
  reference `pvc-downsize-operator`'s own `.gitignore` comment ("dist/chart
  (the Helm chart) is checked in") even though that repo hadn't actually
  committed it yet at the time of this review. Rationale: CI's `helm-lint`
  and the tag-triggered `helm package`/`helm push` job need the chart
  present in a fresh checkout without installing the `kubebuilder` CLI (a
  ~100MB-plus Go module + binary) in CI just to regenerate it.
- **The Helm chart does not label its release namespace with the PSA
  `privileged` labels** `config/default`'s kustomize
  `namespace_psa_patch.yaml` adds (D14: the agent's hostPath mounts need
  them on a PSA-enforcing cluster). A vanilla `helm install
  --create-namespace` namespace carries no labels, and the kubebuilder helm
  plugin doesn't template a `Namespace` object at all (namespace lifecycle
  is left to the installer, standard for this plugin). Not fixed in this
  chunk (out of scope: "New features" / no webhook-equivalent hook exists
  to inject it) — documented in the README's Helm install section and the
  skill's chart workflow instead: label the namespace yourself before
  installing on a PSA-enforcing cluster. kind doesn't enforce PSA by
  default, so the kind smoke e2e passed without this; it's a real gap for
  OpenShift/PSA-`restricted`-by-default clusters, worth a C7 follow-up if
  hardening touches the chart again.
- **CI action versions were already aligned with the reference** (`actions/
  checkout@v4`, `actions/setup-go@v5`, `docker/metadata-action@v5`, `docker/
  login-action@v3`, `docker/{setup-qemu,setup-buildx}-action@v3`, `docker/
  build-push-action@v6`) — checked directly against
  `../pvc-downsize-operator/.github/workflows/ci.yml`, which pins the same
  versions, not the newer `checkout@v5`/`setup-go@v6` the chunk brief
  speculated might be current. No version bumps made; `azure/setup-helm@v4`
  (pinned to `v3.21.3`, matching `.tool-versions`) is newly added for the
  `helm-lint` step (`unit` job) and the new `chart` job, copied verbatim
  from the reference.
- **The hardcoded `blkio-limiter` cluster name in the e2e job's
  diagnostics step is replaced with `$KIND_CLUSTER`**, a workflow-level
  `env:` (`KIND_CLUSTER: blkio-limiter`) matching the Makefile's own
  `KIND_CLUSTER ?= blkio-limiter` default, used for both the `kubectl
  --context kind-${KIND_CLUSTER}` prefix and the `docker exec
  "${KIND_CLUSTER}-worker"` node name.
- **Chart push job** (`chart`, tags only, after `publish`) mirrors the
  reference exactly: bump `dist/chart/Chart.yaml`'s `version`/`appVersion`
  to the git tag, `helm package`, `helm push` to
  `oci://ghcr.io/thomas-maurice/charts`. Untested against a real registry
  push in this chunk (no tag was cut) — same as the reference's own
  unverified-until-first-release state.
- **`config/samples`**: the kubebuilder-scaffolded `iolimiter-sample`/
  `podiolimit-sample` TODO stubs are removed, not kept alongside the new
  ones — they contained no valid spec (`# TODO(user): Add fields here`)
  and would fail the IOLimiter's own CEL "at least one limit" rule if
  applied as-is. `PodIOLimit` is dropped entirely rather than "marked
  internal": SPEC.md §1/D4 already documents it as controller-written/
  agent-written, and a hand-authored sample inviting `kubectl apply -f`
  would contradict that (its `spec` identity fields are CEL-immutable and
  meaningless without a real pod/PVC behind them). Three `IOLimiter`
  samples replace it: a Postgres StatefulSet (`data`+`wal` volumes, D1's
  own worked example), and a namespace-wide baseline +
  `tier: batch`-scoped stricter limiter pair on the same `scratch` volume
  name, demonstrating D2's per-field-minimum merge directly (comments in
  both files cross-reference the resulting effective rule).

### C7 — Hardening
- [x] `ValidatingAdmissionPolicy` + binding: `podiolimits` CREATE/UPDATE/**DELETE** (main resource) only from the controller SA. `podiolimits/status` UPDATE from the agent SA only when `request.userInfo.extra['authentication.kubernetes.io/node-name'][0] == object.spec.nodeName`. SA names and namespace follow kustomize/helm (rendered output checked both ways, `helm template`/`kustomize build`). See D27.
- [ ] Propose the `security-reviewer` pass (manual trigger by the user) -- **out of scope for this chunk's implementation pass, per the director's explicit instruction; the user triggers it separately.**
- [x] e2e: a user with create **and delete** RBAC on `podiolimits` is still denied by the VAP (not RBAC). An agent-SA identity impersonating another node's `node-name` extra is denied on a status write (`kubectl --as` + `--as-user-extra`). `test/e2e/hardening`.

- [x] **Narrow the agent's cgroup mount** (proposed 2026-09-23, user informed): mount only the kubepods subtree (e.g. `/sys/fs/cgroup/kubepods.slice`, kind: `kubelet.slice/kubelet-kubepods.slice`) instead of all of `/sys/fs/cgroup`. The agent then can't reach kubelet/system cgroups (a compromised agent otherwise gives node-wide DoS via memory.max/cgroup.freeze/cgroup.procs). The path is configurable (chart value/flag) and matches discovery (D6); verified on kind. See D28.

**Acceptance.** e2e green (`make e2e`, twice consecutively after a fresh `make deploy-kind`; three consecutive runs observed). Findings are recorded as D-entries here (D27, D28, D29). **Logging:** VAP denials are visible in the e2e output (the apiserver message names the policy, asserted in `test/e2e/hardening`). The review confirms no log line or event leaks anything beyond pod/volume/device/limit identifiers (grepped `internal/agent`, `internal/controller`, `cmd` for `%+v`/`%#v`, whole-object logging, `os.Environ`, `Secret`: none found; every event message already only names volume/device/rule identifiers, matching D26 -- unchanged by this chunk).

**Out of scope.** Webhooks.

**Deviation/implementation notes (found in C7, not guessed).**
- **The security-reviewer pass is explicitly deferred**, per the director's brief for this chunk ("except the security-reviewer pass: the user triggers that manually later"). Not run, not skipped silently.
- **The C6 coordinator review's two findings were folded in during this chunk** (both were assigned to whoever next touched `hack/helm-postprocess.sh`/`dist/chart`, which C7 does for D28's cgroup mount and D27's VAP templates anyway): (1) `hack/checkmanifestparity` (`make check-manifest-parity`, wired into `make verify` and CI's `unit` job) renders both the kustomize and Helm agent DaemonSets and fails loudly on any security-relevant field drift (securityContext, hostPath volumes/mounts) -- proven to actually catch drift, not just pass trivially, by deliberately reintroducing a stale `mountPath` and confirming it fails before fixing it back. (2) `agent-role.yaml`/`agent-rolebinding.yaml`/`agent-metrics-auth-rolebinding.yaml` (plugin-generated from `config/agent/rbac`'s controller-gen markers, not previously touched by the postprocess script) are now wrapped in the same `agent.enabled` guard as the DaemonSet/ServiceAccount, so `agent.enabled=false` no longer leaves them bound to a ServiceAccount the chart never creates.
- **The kubebuilder helm plugin's own "extras" auto-templatization picked up config/vap's `ValidatingAdmissionPolicy`/`Binding` objects too** (the same generic mechanism that emits `templates/extras/agent.yaml` for the DaemonSet), producing a *second*, non-`admissionPolicy.enabled`-gated copy with an invalid `namespace: {{ .Release.Namespace }}` field on a cluster-scoped resource (would be rejected by the apiserver). `hack/helm-postprocess.sh` deletes the plugin's own `templates/extras/podiolimit-*.yaml` copies every run (idempotent) in favour of the hand-templated, gated versions in `templates/rbac/podiolimit-admission-policy.yaml`.
- **kustomize's built-in `nameReference` transformer has no knowledge of `ValidatingAdmissionPolicyBinding.spec.policyName`**: `config/vap/kustomizeconfig.yaml` teaches it, mirroring `config/crd/kustomizeconfig.yaml`'s own pattern for a field kustomize doesn't recognize out of the box. Verified: `kustomize build config/default`'s binding correctly gets the namePrefixed policy name without it needing to be hand-kept in sync.
- **The VAP's own SA-username CEL strings can't be kustomize vars** (kustomize doesn't rewrite arbitrary string fields, only known reference fields): `config/vap`'s two policies hardcode `system:serviceaccount:k8s-blkio-limiter-system:k8s-blkio-limiter-{controller-manager,agent}` literally, with a comment explaining the dependency on `config/default`'s namespace + `namePrefix` + `config/rbac`'s SA names. `test/e2e/hardening` builds the same usernames from the *live* ServiceAccount objects (not by re-hardcoding the string a third time), so a rename surfaces as a clear test failure instead of the VAP silently protecting an identity nobody uses.
- **Shipping the VAPs active by default (not opt-in) broke `test/e2e/agent` and `test/e2e/operator`'s existing `TestDeleteWhileAgentDown`, until fixed** -- both "play the controller" by hand (C3) via the shared `test/e2e/harness` package's `CreatePodIOLimit`/`DeletePodIOLimit` and a few direct `K8sClient.Create/Delete/Patch` calls on `PodIOLimit`, all issued as the ambient kind-admin identity, which the write policy now correctly denies. Fixed by adding `harness.ControllerClient` (the same REST config, but with `Impersonate: rest.ImpersonationConfig{UserName: "system:serviceaccount:...:...-controller-manager"}`) and routing every main-resource mutation (`harness.go`'s `CreatePodIOLimit`/`DeletePodIOLimit`, and the three call sites in `test/e2e/agent` that bypassed those helpers: `release_test.go`'s create/delete, `partial_update_test.go`'s spec patch) through it. This is not a workaround: it's the suite becoming more correct, since "plays the controller by hand" should mean authenticating as the controller too, not just replicating its writes as an arbitrary admin identity. Reads (`Get`/`List`/watches) are unaffected -- the VAP only restricts writes -- and stay on the plain `K8sClient`.
- **The narrowed cgroup mount preserves the host path's relative structure under `--cgroup-root`** (`/host/cgroup/kubelet.slice/kubelet-kubepods.slice`, not flattened to `/host/cgroup`), specifically so `internal/cgroup.DiscoverKubepods`'s existing candidate search needs **no code change**: only `internal/agent/agent.go`'s two `cgroup.controllers` preflight checks (startup, and D24's periodic self-check) needed to move from `--cgroup-root` to the *discovered* `layout.Base`, since cfg.cgroupRoot itself is now an empty mountpoint-stub directory rather than a real cgroup2 mount. `selfCheckPasses`/`runSelfCheckLoop` gained a `kubepodsRoot string` parameter for this; no test in `internal/agent/agent_test.go` covered these functions directly (grepped to confirm), so the signature change needed no test updates.

**Fix batch (2026-09-23, post-C7, from user reports + the C7 review).** Not a new chunk -- corrections to shipped C7/C4 behaviour, done together since they share `hack/helm-postprocess.sh`/`config/vap`/`internal/controller`.
- **D27 revised: DELETE removed from `podiolimit-write-restricted`** (`config/vap/podiolimit_write_policy.yaml`, `hack/helm-postprocess.sh`'s templated copy). Reported bugs: a cluster-admin's routine `kubectl delete podiolimit` was denied ("forbidden: ValidatingAdmissionPolicy ... write-restricted"), and, more seriously, `system:serviceaccount:kube-system:generic-garbage-collector`'s ownerRef cleanup and the namespace controller's own sweep were denied too -- with the controller down, a namespace containing a limited pod could hang `Terminating` indefinitely. See D27's rewritten rationale. `test/e2e/hardening.TestVAP_AdminCanDeletePodIOLimit` (ambient cluster-admin deletes a real, applied `PodIOLimit`; the D9 finalizer still resets `io.max` first, proven via the agent's `IOLimitReset` Pod event) and `test/e2e/operator.TestNamespaceDeleteConvergesWithControllerUp` / `TestNamespaceDeleteConvergesOnceControllerRestored` (the latter reproduces the exact reported failure mode: controller scaled to 0, namespace delete must stay `Terminating` -- not error, not silently succeed -- until the controller is scaled back up, then converge with no further intervention) cover this. `test/e2e/hardening.TestVAP_DeleteAlsoDenied` (the old C7 test asserting the opposite) was replaced, not kept alongside: it tested behaviour this fix batch deliberately reverses.
- **D30 (new): orphan `PodIOLimit` garbage collection.** Reported bug: a leftover `PodIOLimit` with no ownerRef, pointing at a pod UID that doesn't exist, was never reconciled -- `PodReconciler` only ever learned about a `PodIOLimit` via `Owns()` (ownerRef-driven) or indirectly through an `IOLimiter`/PVC event resolving to a live pod. `internal/controller/pod_controller.go`'s `SetupWithManager` now also `Watches(&PodIOLimit{}, ...)` directly, mapped by the object's own `(namespace, spec.podName)` (D16's existing field index) regardless of ownerRef; `cmd/main.go`'s manager cache gained an explicit bounded `SyncPeriod` (10m, was the controller-runtime default of 10h) as a second, independent guarantee. The existing D9 dead-pod path in `Reconcile` (step 2) already does the right thing once an orphan is enqueued: delete + release the finalizer without waiting on the agent, since there's no live cgroup. See D30 for the chosen race fix (a live `APIReader` fallback, not an age threshold) and its justification. Unit (`TestPodReconciler_OrphanPodIOLimit_NoOwnerRef_Released`, `TestPodReconciler_LiveReadAvoidsMisidentifyingNewPodAsOrphan`, `TestMapPodIOLimitToPod`), envtest (`TestPodReconciler_Envtest_OrphanPodIOLimit_NoOwnerRef_Released`) and e2e (`test/e2e/hardening.TestOrphanPodIOLimitGarbageCollected`, created as the controller SA via `ControllerClient` with a bogus pod UID and no ownerRef, asserted gone within 30s) all cover this.
- **VAP `apiVersions` changed from `["v1alpha1"]` to `["*"]`** on both policies (`config/vap`, `hack/helm-postprocess.sh`): a pinned list is a maintenance trap that silently stops protecting the resource the day a new version is served. `"*"` uses the same wildcard semantics as an admission webhook rule (`NamedRuleWithOperations.apiVersions`). `test/e2e/hardening.TestVAPPolicies_MatchAllServedVersions` reads the live CRD's served versions and the live VAPs' `apiVersions` and fails if a VAP doesn't cover every served version (skipped -- passes trivially -- only when the VAP already uses the `"*"` wildcard, which both do).
- **`PodIOLimit` documented as internal, end to end**: the type doc comment (`api/v1alpha1/podiolimit_types.go`) now reads "Internal: created and managed by the IOLimiter controller, one per limited pod. Do not create or edit by hand; use IOLimiter.", `nodeName`/`podName`'s field docs say the same, and the printer columns were reordered/regated so `kubectl get pil` (no `-o wide`) shows `Pod`, `Ready`, `Age` by default -- `Node` (the field that confused the reported user) and a new `Managed-By` column (`.metadata.labels['app.kubernetes.io/managed-by']`, real data, not a phantom column) both move to `priority: 1` (wide-only). `+kubebuilder:resource:categories=all` was never set on `PodIOLimit` (confirmed by grepping the generated CRD before this fix batch too) -- nothing to remove there. Regenerated via `make manifests generate` (idempotent, verified twice) and `make helm-chart`.

**Fix batch (2026-09-24, chaos testing + D35).** Not a new chunk -- agent/controller residuals plus D35's implementation.
- **Agent event-loss residual fixed** (`internal/agent/reconciler.go`, `applyDevice`): a conflict on the *final* status write left a transition computed but never persisted (correctly, per D26: events only fire after the write succeeds); the retried reconcile then saw the kernel already matching the desired rule (`curLine == line`) and, before this fix, treated that unconditionally as a no-op resync -- silently losing the event forever. Fixed by reusing the existing `wasApplied` check (status already recorded Applied with this exact rule) inside that branch: only a genuine `wasApplied` match stays a silent no-op; otherwise the same Applied/Updated event choice as a real kernel write fires. `TestReconcile_ConflictedFinalWrite_EventReemittedExactlyOnceOnRetry` proves the event fires exactly once on the retry and nothing fires on the next steady-state resync.
- **Controller residual fixed** (`internal/controller/pod_controller.go`): `recordPodIOLimitWriteFailure`/`clearPodIOLimitWriteFailure`'s own `Status().Update` on the IOLimiter could conflict and was logged-and-dropped, letting a stale `PodIOLimitWriteFailed` condition stick. Both now return a `conflict bool`; `createPodIOLimit`/`patchIfDiffer` requeue (`RequeueAfter: requeueSoon`) instead of falling through on conflict. `TestPodReconciler_CreateDenied_RecordConflictRequeues`, `TestPodReconciler_ClearWriteFailure_ConflictRequeues`.
- **NIT fixed** (`internal/agent/reconciler.go`, `cacheHasObjects`): a transient `List` error now logs at V(1) and keeps the previous known `hasObjects` value (mutex-guarded field on the reconciler) instead of returning `false`, so it can't wrongly flip D24 liveness state. `TestCacheHasObjects_ListError_KeepsPreviousValue`.
- **D35 implemented** (`internal/controller/desired/compute.go`, `pod_controller.go`, `iolimiter_controller.go`): `Compute` gained an `existing []PodVolumeLimit` parameter (the pod's current PodIOLimit spec, or nil). For a (limiter, volume) row with an InvalidLimit field, if that limiter was already listed in `existing`'s `Sources` for that exact volume, the volume's entire existing merged `Limits` is merged in as an additional per-field-minimum floor (D2), and the limiter stays listed in `Sources`; the Issue message appends `"; previous limits kept"`. A limiter never previously a source for that volume (new match, or a volume it never touched) gets no floor -- unchanged pre-D35 behaviour. This is the documented conservative "whole-volume floor" option D35 itself sanctions (exact per-limiter-field attribution isn't recoverable from Compute's merged output). Both `PodReconciler` and `IOLimiterReconciler` now look up the pod's existing PodIOLimit before calling `Compute` (`existingPodIOLimitVolumes`/`computeVolumesFor`, the latter factored out of `IOLimiterReconciler.Reconcile` to keep its cyclomatic complexity within `gocyclo`'s 30 limit). Unit tests: `TestCompute_D35_*` (freeze, fix-resumes, never-a-source-no-floor, per-field-min-against-floor in both directions). Controller test: `TestPodReconciler_D35_InvalidLimitFreezesPodIOLimitSpec` (5Mi valid → 100m invalid keeps spec at 5Mi and IOLimiter reports InvalidLimit → 8Mi fixed resumes). E2E: `test/e2e/operator.TestInvalidLimitFreezesInsteadOfLoosening` (real kind cluster: edits a live limiter to `100m`, asserts io.max keeps the previous wbps over a sustained poll, then fixes it).
- **Dead-pod release NotFound residual fixed** (`internal/controller/finalizer.go`, `removeFinalizerOptimistic`): the initial `Get` already treated a NotFound `PodIOLimit` as "already gone" (success), but the `Patch` below it did not -- if GC's own delete won a race against this reconciler's own release path (chaos finding: `Reconciler error ... podiolimits... not found`, a pod deleted while the agent was down), the Patch's NotFound propagated as a real reconcile error with controller-runtime backoff. Now treated identically to the Get's NotFound: V(1) "already gone", `return true, nil`. `TestPodReconciler_FinalizerRemovalNotFound_TreatedAsSuccess`.
- **Agent duplicate Info log fixed** (`internal/agent/reconciler.go`, `logResolution`): for any volume sharing a device with another (D7's `SharedDevice`), the persisted `status.volumes[].reason` is `"SharedDevice"` (set by the apply loop) but a freshly resolved volume's own `Reason` is always `""` (`resolveVolume` never sets one for a successful resolution) -- comparing them directly never matched, logging "Volume resolution" at Info on *every* reconcile forever for such a pod, not just once as originally reported. Fixed: `Reason` is only compared when `res.State != ""` (i.e. only for the Pending/Unsupported cases resolution itself decides); a resolved volume's unchanged-ness is judged on state/device/mountDevice alone. `TestReconcile_SharedDevice_ResolutionLoggedOnceNotEveryResync`.
- **Out-of-scope pre-existing race found and fixed to unblock operator e2e** (`internal/controller/pod_controller.go`, `createPodIOLimit`): not one of the six assigned items, discovered running the full `test/e2e/operator` suite after the above. Two Pod reconciles for the same pod can fire back-to-back (the IOLimiter's own status write, reacting to the same Pod event, re-enqueues the pod via `mapIOLimiterToPods` almost immediately after a successful `Create`); the second reconcile's cached `List`-by-podName (Step 1/2) can still read the PodIOLimit as absent (informer propagation lag), so its own `Create` 409s `AlreadyExists`. Before this fix that was recorded as a permanent `PodIOLimitWriteFailed` condition (nothing ever re-triggers this pod's reconcile to clear it, since nothing about the object changes again) -- observed on kind causing `IOLimiter` status to wedge at `Failed` indefinitely. Fixed: `apierrors.IsAlreadyExists(err)` is treated as benign (V(1) log, `ctrl.Result{}, nil`, no condition, no event) -- the object's own Create event already re-triggers a fresh reconcile via the existing `PodIOLimit` watch (D30). `TestPodReconciler_CreateAlreadyExists_NotSurfacedAsFailure`.

### C8 — Legacy removal
- [x] Delete `cmd/k8s-blkio-limiter`, `internal/limiter`, `charts/`, `dev/`, `scripts/` (moved to `hack/` in C3), `.github/workflows/{build,integration}.yaml`, `TEST_REPORT.md` (superseded by e2e). Keep `.ideas/`.
- [x] `go mod tidy`. Check for stale references with `grep -r "blkio-limiter.maurice.fr/"`.

**Acceptance.** `make verify` green. **Logging:** the legacy `slog` JSON logger is gone with the legacy code, and `grep -r "log/slog"` outside the vendor tree is empty (a single logging stack remains). No annotation-mode strings left outside README's migration section.

**Deviation/implementation notes.**
- `make e2e` was **not** run for this chunk (explicit instruction: don't touch the kind cluster, it's about to be torn down). `make verify`/`make test`/`make lint`/`go build ./...` are the acceptance gate actually exercised here; a full `make e2e` should still be run before the next kind-cluster-dependent chunk to confirm nothing in `test/e2e/*` regressed from the deletions (it shouldn't have — none of the deleted trees were imported by `test/e2e/*` or `internal/{agent,controller}`, confirmed by `go build ./...` and `go vet ./...` passing clean).
- Stale references fixed beyond the two greps named in the acceptance line: `.gitignore` (`TEST_REPORT.md`, `dev/test-longhorn.yaml`, `dev/test-sts.yaml`), `AGENTS.md` and `.claude/skills/working-here/SKILL.md` (both had a "Legacy code" layout row/test-command paragraph pointing at the now-deleted trees), README's migration section (`is being removed in C8` → `was removed in C8`, past tense). `test/e2e/agent/two_devices_test.go` and `internal/iomax/iomax_test.go` still reference `internal/limiter/apply.go` in comments explaining *why* a regression test exists (the K1 multi-line-write bug) — left as-is: historical rationale, not a functional or doc reference, and outside the file list this chunk's acceptance criteria named.

## 12. Open questions — all resolved (user, 2026-09-23)

1. **Trust model → (a) admin-imposed, namespaced `IOLimiter`.** Editor/admin helper roles are shipped **without** `aggregate-to-edit/admin` labels.
2. **Minimum Kubernetes → (a) 1.32+.** CRD selectable field `spec.nodeName`, VAP with the node-name claim.
3. **hostPath / emptyDir → (a) unsupported, reported on status** (D19).
4. **Heartbeat → (a) none.** Status is written on change only.

## 13. Deliberately not built

- NRI plugin / OCI blockIO (D25).
- `memory.high` management for dirty-page OOM (`.ideas/dirty-pages-and-oom.md`): separate feature.
- `ClusterIOLimiter`, wildcard "all PVC volumes", block-mode PVCs, btrfs (major-0 subvolumes), `io.weight`/`io.latency`.
