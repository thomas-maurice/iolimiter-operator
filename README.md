# k8s-blkio-limiter

[![Build and Push](https://github.com/thomas-maurice/k8s-blkio-limiter/actions/workflows/build.yaml/badge.svg)](https://github.com/thomas-maurice/k8s-blkio-limiter/actions/workflows/build.yaml)

A Kubernetes DaemonSet that applies cgroup v2 `io.max` IOPS/bandwidth limits to containers based on pod annotations.

## Quick start (kind)

```bash
# Everything in one command: creates cluster, sets up loop device, builds,
# loads, deploys the DaemonSet, runs the test pod and tails its logs.
make up
```

Or step by step:

```bash
make kind-create     # create kind cluster (with /dev passthrough)
make setup-loopdev   # create a loopback block device inside the kind node
make kind-load       # build the Docker image and load into kind
make deploy          # helm install into kube-system
make test            # deploy annotated test pod and tail its logs
```

## How it works

1. DaemonSet lists pods on its node via the Kubernetes API.
2. Finds pods with the `blkio-limiter.maurice.fr/limit` annotation.
3. Resolves container ID -> cgroup path -> PID -> `/proc/<pid>/mountinfo` -> block device `major:minor`.
4. Writes the `io.max` rule to the container's cgroup.
5. Polls every 5s. Resets limits when annotations are removed. Recovers orphaned rules on startup.

## Annotations

```yaml
metadata:
  annotations:
    # Required: comma-separated io.max parameters
    blkio-limiter.maurice.fr/limit: "riops=100,wiops=50,rbps=10485760,wbps=5242880"

    # Required: mount path inside the container whose backing device to limit.
    blkio-limiter.maurice.fr/volume-path: "/data"
```

| Parameter | Meaning                    | Unit          |
|-----------|----------------------------|---------------|
| `riops`   | Max read IOPS              | ops/sec       |
| `wiops`   | Max write IOPS             | ops/sec       |
| `rbps`    | Max read bandwidth         | bytes/sec     |
| `wbps`    | Max write bandwidth        | bytes/sec     |

## Local development

### Prerequisites

- Docker, kind, kubectl, Go 1.22+, Helm 3

### Loop device

Kind nodes use overlay filesystems (major 0) which `io.max` cannot throttle. `scripts/setup-loopdev.sh` creates a 64MB loopback block device inside the kind node so there's a real device to test against. On real clusters (GKE, EKS, AKS), PVCs are backed by real block devices and this isn't needed.

### Helm chart

```bash
# Install (local dev with kind)
helm upgrade --install k8s-blkio-limiter charts/k8s-blkio-limiter \
  -n kube-system -f dev/values-kind.yaml

# Install (production — defaults to GHCR image)
helm upgrade --install k8s-blkio-limiter charts/k8s-blkio-limiter \
  -n kube-system --set image.tag=v0.1.0

# Uninstall
helm uninstall k8s-blkio-limiter -n kube-system
```

## Make targets

```
make help            # list all targets
make up              # full setup: cluster -> loopdev -> build -> deploy -> test
make down            # tear everything down
make test-unit       # run Go unit tests
make logs            # tail DaemonSet logs
make test-status     # one-shot status check
make shell-node      # shell into the kind node
make cgroup-tree     # show all io.max files on the node
make node-mounts     # show block device mounts on the node
```

## Project structure

```
cmd/k8s-blkio-limiter/main.go    # Entry point
internal/limiter/
  limiter.go                      # Limiter struct, New(), Run()
  reconcile.go                    # Pod listing, annotation validation, cache
  cgroup.go                       # Cgroup tree walking, PID resolution, orphan recovery
  device.go                       # Block device resolution, io.max parsing
  apply.go                        # io.max writing, reset, container ID helpers
  types.go                        # Types and constants
  *_test.go                       # Unit tests
charts/k8s-blkio-limiter/        # Helm chart
scripts/setup-loopdev.sh          # Loop device setup for kind
dev/                              # Kind cluster config, test pod, values overlay
```

## Requirements

- cgroups v2 on the host (default on all modern distros and managed k8s)
- systemd cgroup driver (default for containerd)
