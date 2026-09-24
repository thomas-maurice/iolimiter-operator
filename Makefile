# Image URL to use all building/pushing image targets. Local dev default; the
# published image is ghcr.io/thomas-maurice/iolimiter-operator (see CI). One
# image, two modes: /manager (controller) and /manager agent (node agent).
IMG ?= iolimiter-operator:dev
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

# D15: two separate controller-gen RBAC passes, one per component, so the
# agent's markers (least privilege: podiolimits/status only) never merge
# into the controller's role and vice versa. CRD/webhook generation stays a
# single pass over ./... (no overlapping markers there).
.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role paths="./internal/controller/...;./cmd/..." output:rbac:artifacts:config=config/rbac
	"$(CONTROLLER_GEN)" rbac:roleName=agent-role paths="./internal/agent/..." output:rbac:artifacts:config=config/agent/rbac
	"$(CONTROLLER_GEN)" crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# CGO_ENABLED=0: client-go drags in the cgo net resolver, and the nix cc
# toolchain can't link -lresolv on darwin. Neither the manager nor its tests
# need cgo.
.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run unit + envtest tests (excludes test/e2e).
	CGO_ENABLED=0 KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /test/e2e) -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: verify
verify: lint test build check-manifest-parity ## Lint, test, build and manifest-parity - the fast local/CI gate.

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	CGO_ENABLED=0 go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# Override BASE_IMAGE to build from another registry, e.g.
# make docker-build IMG=<img> BASE_IMAGE=docker.io/library/golang:1.26
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name iolimiter-operator-builder
	$(CONTAINER_TOOL) buildx use iolimiter-operator-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(if $(BASE_IMAGE),--build-arg BASE_IMAGE=$(BASE_IMAGE)) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm iolimiter-operator-builder
	rm Dockerfile.cross

# F10 (Fable review): render config/default with an image override via a
# throwaway overlay in a temp dir instead of `kustomize edit set image`
# against the tracked config/default/kustomization.yaml -- the latter left
# that file permanently mutated to whatever IMG a prior `make
# build-installer`/`deploy` run used (usually the local :dev tag), so the
# checked-in default a real install (`kubectl apply -k config/default`)
# picks up depended on Makefile history instead of being the published
# image. $(1) = the kustomize root to wrap (config/default directly for
# build-installer's public installer output, hack/kind-overlay for
# deploy/deploy-kind's kind cgroup override).
# A tmpdir under $(CURDIR)/bin (wholly gitignored, never a real target) --
# not $(TMPDIR)/mktemp -d: kustomize refuses an absolute resource path
# ("new root ... cannot be absolute"), and a relative path from a
# system-tmp dir back to this repo has no fixed number of ".." segments.
# From bin/.deploy-overlay-$$$$, "../../$(1)" always lands back on this
# repo's own $(1).
define kustomize_build_with_image
tmpdir="$(CURDIR)/bin/.deploy-overlay-$$$$"; \
mkdir -p "$$tmpdir"; \
trap 'rm -rf "$$tmpdir"' EXIT; \
full_img="${IMG}"; \
img_name="$${full_img%:*}"; \
img_tag="$${full_img##*:}"; \
printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- ../../%s\nimages:\n- name: ghcr.io/thomas-maurice/iolimiter-operator\n  newName: %s\n  newTag: %s\n' \
	"$(1)" "$$img_name" "$$img_tag" > "$$tmpdir/kustomization.yaml"; \
"$(KUSTOMIZE)" build "$$tmpdir"
endef

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	# D14/§8: the image transform must cover config/default, not just
	# config/manager -- config/manager only covers the Deployment,
	# config/default also covers the agent DaemonSet (config/agent), so
	# setting it only in config/manager would silently leave the agent on
	# the stale image.
	set -euo pipefail; $(call kustomize_build_with_image,config/default) > dist/install.yaml

##@ Helm

.PHONY: helm-chart
# The helm plugin runs `make build-installer` itself (IMG from the env), so the chart's
# default image is whatever IMG was at that point: always render it with the
# published image, never the local :dev one.
PUBLISHED_IMG ?= ghcr.io/thomas-maurice/iolimiter-operator:latest
helm-chart: ## Regenerate the Helm chart (dist/chart) from kustomize config via the kubebuilder helm plugin.
	IMG=$(PUBLISHED_IMG) kubebuilder edit --plugins=helm/v2-alpha --force
	# §8: the plugin only templatizes the manager Deployment. Idempotent.
	hack/helm-postprocess.sh

.PHONY: helm-lint
helm-lint: ## Lint the generated Helm chart.
	helm lint dist/chart

# Coordinator review (C6->C7): the chart's agent DaemonSet
# (hack/helm-postprocess.sh) is a hand-maintained duplicate of
# config/agent/agent.yaml, not derived from it -- a security-relevant edit
# to one (securityContext, hostPath volumes/mounts) can land without the
# other ever being touched. This renders both and fails loudly on drift.
.PHONY: check-manifest-parity
check-manifest-parity: kustomize ## Fail if the kustomize and Helm agent DaemonSets disagree on any security-relevant field.
	KUSTOMIZE_BIN="$(KUSTOMIZE)" HELM_BIN=helm CGO_ENABLED=0 go run ./hack/checkmanifestparity

##@ Deployment

# Every kubectl/helm call in this Makefile goes through the pinned kind
# context - never the ambient one. A wrong-context apply is the failure mode
# we are designing out.
KIND_CLUSTER ?= blkio-limiter
KCTL := kubectl --context kind-$(KIND_CLUSTER)
HELM := helm --kube-context kind-$(KIND_CLUSTER)

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the pinned kind cluster.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KCTL) apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the pinned kind cluster. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | $(KCTL) delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the pinned kind cluster.
	# F2: hack/kind-overlay layers kind's cgroup hostPath override on top of
	# config/default -- every Makefile deploy target only ever targets the
	# pinned kind cluster (see SKILL.md), so this is the right default here
	# even though config/default's own checked-in default (used by a real,
	# non-kind `kubectl apply -k config/default`) is now kubepods.slice
	# (F2's fix).
	set -euo pipefail; $(call kustomize_build_with_image,hack/kind-overlay) | $(KCTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the pinned kind cluster. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | $(KCTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Kind / E2E
#
# hack/kind-config.yaml, hack/setup-loopdev.sh, hack/e2e-storage.yaml and the
# test/e2e harness land in C3; the targets below are the pinned
# cluster-lifecycle skeleton they'll use (D13: no cert-manager - nothing here
# needs admission webhooks).

.PHONY: kind-create
kind-create: ## Create the pinned kind cluster (1 control-plane + 1 worker) if it doesn't exist.
	@kind get clusters | grep -qx '$(KIND_CLUSTER)' \
		|| kind create cluster --name $(KIND_CLUSTER) --config hack/kind-config.yaml

.PHONY: kind-delete
kind-delete: ## Delete the pinned kind cluster.
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: kind-load
kind-load: ## Load $(IMG) into the pinned kind cluster.
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)

# busybox:1.37 is pulled once during kind-setup so no test pulls it from a
# registry during the e2e run (§10). `kind load docker-image` (docker save |
# ctr import --all-platforms) is unreliable against a local multi-platform
# manifest list cached by Docker Desktop's containerd image store (the
# archive references other-platform blobs the local store never fetched,
# and ctr import then fails with "content digest ... not found" even though
# the current platform's layer is present); pulling directly into each
# node's own containerd avoids that archive round-trip entirely and works
# the same way in CI.
BUSYBOX_IMG ?= docker.io/library/busybox:1.37

.PHONY: kind-load-busybox
kind-load-busybox: ## Pre-pull the e2e busybox image into every kind node's containerd.
	for node in $(KIND_CLUSTER)-control-plane $(KIND_CLUSTER)-worker; do \
		docker exec $$node ctr --namespace=k8s.io images pull $(BUSYBOX_IMG); \
	done

.PHONY: kind-loopdev
kind-loopdev: ## Create/verify the e2e loop devices on the worker node (idempotent, §10).
	docker exec $(KIND_CLUSTER)-worker mkdir -p /hack
	docker cp hack/setup-loopdev.sh $(KIND_CLUSTER)-worker:/hack/setup-loopdev.sh
	docker exec $(KIND_CLUSTER)-worker bash /hack/setup-loopdev.sh

.PHONY: kind-storage
kind-storage: ## Apply the e2e StorageClass (blkio-local).
	$(KCTL) apply -f hack/e2e-storage.yaml

.PHONY: kind-setup
kind-setup: kind-create kind-loopdev kind-storage kind-load-busybox ## Create the pinned kind cluster + loop devices + storage class + busybox image.

.PHONY: kind-reset
kind-reset: kind-delete kind-setup ## Delete and recreate the pinned kind cluster from scratch.

.PHONY: deploy-kind
deploy-kind: docker-build kind-load deploy ## Build, load and deploy the manager image into the pinned kind cluster.
	@# The tag (:dev) doesn't change between builds: force new pods onto the freshly loaded image.
	$(KCTL) -n iolimiter-operator-system rollout restart deploy/iolimiter-operator-controller-manager
	$(KCTL) -n iolimiter-operator-system rollout status deploy/iolimiter-operator-controller-manager --timeout=3m

.PHONY: e2e-setup
e2e-setup: kind-setup deploy-kind ## One-time setup for e2e: cluster + deployed manager.

E2E_TIMEOUT ?= 30m

.PHONY: e2e
e2e: ## Run the e2e suite (test/e2e/agent, test/e2e/operator, test/e2e/hardening; build tag e2e) against the prepared pinned cluster.
	# -p 1: test/e2e/agent and test/e2e/operator mutate the same cluster-wide
	# state (the controller-manager Deployment's replica count, the agent
	# DaemonSet's nodeSelector) via their own TestMain. `go test ./...`
	# otherwise builds/runs multiple packages' test binaries concurrently
	# (bounded by GOMAXPROCS, independent of -count=1), which races the two
	# packages' setup/teardown against each other. -p 1 forces the two
	# packages to run one after the other, never overlapping.
	KIND_CLUSTER=$(KIND_CLUSTER) CGO_ENABLED=0 go test -tags=e2e -timeout=$(E2E_TIMEOUT) -count=1 -p 1 -v ./test/e2e/...

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
# KUBECTL/KIND/HELM are the ambient binaries used to build the pinned wrappers
# above (KCTL/HELM) and the kind lifecycle targets; install/pin them via
# .tool-versions (kind, helm) or your Go toolchain (kubectl).
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.22.0

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.13.1
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		CGO_ENABLED=0 $(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
CGO_ENABLED=0 GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Helm Deployment

## Helm binary to use for deploying the chart
HELM ?= helm
## Namespace to deploy the Helm release
HELM_NAMESPACE ?= iolimiter-operator-system
## Name of the Helm release
HELM_RELEASE ?= iolimiter-operator
## Path to the Helm chart directory
HELM_CHART_DIR ?= dist/chart
## Additional arguments to pass to helm commands
HELM_EXTRA_ARGS ?=

.PHONY: install-helm
install-helm: ## Install the latest version of Helm.
	@command -v $(HELM) >/dev/null 2>&1 || { \
		echo "Installing Helm..." && \
		curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash; \
	}

.PHONY: helm-deploy
helm-deploy: install-helm ## Deploy manager to the K8s cluster via Helm. Specify an image with IMG.
	IMG="$(IMG)"; $(HELM) upgrade --install $(HELM_RELEASE) $(HELM_CHART_DIR) \
		--namespace $(HELM_NAMESPACE) \
		--create-namespace \
		--set manager.image.repository=$${IMG%:*} \
		--set manager.image.tag=$${IMG##*:} \
		--wait \
		--timeout 5m \
		$(HELM_EXTRA_ARGS)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall the Helm release from the K8s cluster.
	$(HELM) uninstall $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-status
helm-status: ## Show Helm release status.
	$(HELM) status $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-history
helm-history: ## Show Helm release history.
	$(HELM) history $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)

.PHONY: helm-rollback
helm-rollback: ## Rollback to previous Helm release.
	$(HELM) rollback $(HELM_RELEASE) --namespace $(HELM_NAMESPACE)
