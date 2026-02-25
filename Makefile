IMAGE        := k8s-blkio-limiter:local
KIND_CLUSTER := k8s-blkio-limiter-test
HELM_RELEASE := k8s-blkio-limiter
HELM_NS      := kube-system

# ─── Build ───────────────────────────────────────────────────────────────────

.PHONY: build
build: ## Build the k8s-blkio-limiter Docker image
	docker build -t $(IMAGE) .

# ─── Test ────────────────────────────────────────────────────────────────────

.PHONY: test-unit
test-unit: ## Run unit tests
	go test ./...

# ─── Kind cluster ────────────────────────────────────────────────────────────

.PHONY: kind-create
kind-create: ## Create a kind cluster with /dev passthrough
	@if kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "kind cluster '$(KIND_CLUSTER)' already exists"; \
	else \
		kind create cluster --name $(KIND_CLUSTER) --config dev/kind-config.yaml --wait 60s; \
	fi

.PHONY: kind-load
kind-load: build ## Build image and load it into the kind cluster
	kind load docker-image $(IMAGE) --name $(KIND_CLUSTER)

.PHONY: kind-delete
kind-delete: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

# ─── Loop device setup ───────────────────────────────────────────────────────

.PHONY: setup-loopdev
setup-loopdev: ## Create a loopback block device inside the kind node
	@echo "=== Copying setup script into kind node ==="
	docker exec $(KIND_CLUSTER)-control-plane mkdir -p /scripts
	docker cp scripts/setup-loopdev.sh $(KIND_CLUSTER)-control-plane:/scripts/setup-loopdev.sh
	@echo ""
	@echo "=== Running setup inside kind node ==="
	docker exec $(KIND_CLUSTER)-control-plane bash /scripts/setup-loopdev.sh

# ─── Deploy ──────────────────────────────────────────────────────────────────

.PHONY: deploy
deploy: ## Deploy via Helm (uses values-kind.yaml overlay for local dev)
	helm upgrade --install $(HELM_RELEASE) charts/k8s-blkio-limiter \
		-n $(HELM_NS) --create-namespace \
		-f dev/values-kind.yaml
	@echo ""
	@echo "Waiting for DaemonSet to be ready..."
	kubectl -n $(HELM_NS) rollout status daemonset/$(HELM_RELEASE) --timeout=60s

.PHONY: undeploy
undeploy: ## Remove the Helm release
	helm uninstall $(HELM_RELEASE) -n $(HELM_NS) --ignore-not-found

# ─── Integration test ────────────────────────────────────────────────────────

.PHONY: test
test: ## Deploy the test pod and tail its logs
	kubectl delete pod io-limit-test --ignore-not-found --wait=true 2>/dev/null || true
	kubectl apply -f dev/test-pod.yaml
	@echo ""
	@echo "Waiting for test pod to start..."
	kubectl wait --for=condition=Ready pod/io-limit-test --timeout=30s
	@echo ""
	@echo "Tailing logs (Ctrl-C to stop)..."
	@echo "Watch for io.max to change from empty to your limits."
	@echo ""
	kubectl logs -f io-limit-test

.PHONY: test-status
test-status: ## Show current io.max of the test pod (one-shot)
	@echo "=== Test pod annotations ==="
	@kubectl get pod io-limit-test -o jsonpath='{.metadata.annotations}' | python3 -m json.tool 2>/dev/null || \
		kubectl get pod io-limit-test -o jsonpath='{.metadata.annotations}'
	@echo ""
	@echo ""
	@echo "=== k8s-blkio-limiter logs (last 20 lines) ==="
	@kubectl -n $(HELM_NS) logs -l app.kubernetes.io/name=k8s-blkio-limiter --tail=20
	@echo ""
	@echo "=== Test pod logs (last 10 lines) ==="
	@kubectl logs io-limit-test --tail=10

# ─── All-in-one ──────────────────────────────────────────────────────────────

.PHONY: up
up: kind-create setup-loopdev kind-load deploy test ## Full setup: cluster -> loopdev -> build -> load -> deploy -> test

.PHONY: down
down: undeploy kind-delete ## Tear everything down

# ─── Debug ───────────────────────────────────────────────────────────────────

.PHONY: logs
logs: ## Tail k8s-blkio-limiter DaemonSet logs
	kubectl -n $(HELM_NS) logs -l app.kubernetes.io/name=k8s-blkio-limiter -f

.PHONY: shell-node
shell-node: ## Open a shell on the kind node to inspect cgroups
	docker exec -it $(KIND_CLUSTER)-control-plane bash

.PHONY: cgroup-tree
cgroup-tree: ## Show all io.max files on the kind node
	docker exec $(KIND_CLUSTER)-control-plane \
		find /sys/fs/cgroup -maxdepth 6 -name "io.max" -exec sh -c 'echo "--- {} ---" && cat {}' \;

.PHONY: node-mounts
node-mounts: ## Show block device mounts on the kind node
	docker exec $(KIND_CLUSTER)-control-plane findmnt -t ext4,xfs,btrfs

# ─── Help ────────────────────────────────────────────────────────────────────

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
