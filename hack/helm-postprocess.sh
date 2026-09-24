#!/usr/bin/env bash
# Post-processes the chart the kubebuilder helm/v2-alpha plugin regenerates
# (`make helm-chart`, `dist/chart`, see SPEC.md §8).
#
# The plugin only fully templatizes the manager Deployment. What it leaves
# broken/unwired for the agent DaemonSet (verified against the generated
# output, not guessed):
#   - dist/chart/templates/extras/agent.yaml has the image, args, resources,
#     tolerations, nodeSelector, securityContext and priorityClassName all
#     hardcoded: `--set manager.image.tag=X`/agent values have no effect on
#     the agent's image.
#   - The DaemonSet's `serviceAccountName` references a
#     "<release>-agent" ServiceAccount that the chart never creates (the
#     plugin only emits one ServiceAccount, for the manager) - installing the
#     unmodified chart leaves the agent pod unable to start (no such SA).
#
# This script rewrites both files from scratch each run, so it's idempotent:
# re-running `make helm-chart` (which always regenerates dist/chart from
# kustomize output first) and then this script produces the same result.
#
# It intentionally leaves every other generated file (CRDs, manager
# Deployment, RBAC, network policy, etc.) untouched: the plugin already
# templatizes those correctly.
set -euo pipefail

CHART_DIR="${CHART_DIR:-dist/chart}"
AGENT_TEMPLATE="$CHART_DIR/templates/extras/agent.yaml"
AGENT_SA_TEMPLATE="$CHART_DIR/templates/rbac/agent-serviceaccount.yaml"
VALUES_FILE="$CHART_DIR/values.yaml"

if [[ ! -f "$AGENT_TEMPLATE" ]]; then
  echo "helm-postprocess: $AGENT_TEMPLATE not found (did the helm plugin stop generating an agent template?)" >&2
  exit 1
fi

cat > "$AGENT_TEMPLATE" <<'EOF'
{{- if or (not (hasKey .Values.agent "enabled")) (.Values.agent.enabled) }}
apiVersion: apps/v1
kind: DaemonSet
metadata:
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
    control-plane: agent
  name: {{ include "iolimiter-operator.resourceName" (dict "suffix" "agent" "context" $) }}
  namespace: {{ .Release.Namespace }}
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
      control-plane: agent
  template:
    metadata:
      annotations:
        kubectl.kubernetes.io/default-container: agent
      labels:
        app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
        helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
        app.kubernetes.io/instance: {{ .Release.Name }}
        app.kubernetes.io/managed-by: {{ .Release.Service }}
        control-plane: agent
    spec:
      {{- with .Values.agent.priorityClassName }}
      priorityClassName: {{ . | quote }}
      {{- end }}
      {{- with .Values.agent.tolerations }}
      tolerations: {{ toYaml . | nindent 8 }}
      {{- end }}
      {{- with .Values.agent.nodeSelector }}
      nodeSelector: {{ toYaml . | nindent 8 }}
      {{- end }}
      containers:
      - args:
        - agent
        - --health-probe-bind-address=:8081
        - --metrics-bind-address=:8443
        - --resync-period={{ .Values.agent.resyncPeriod }}
        {{- if .Values.agent.allowRootDevice }}
        - --allow-root-device=true
        {{- end }}
        {{- with .Values.agent.protectedPaths }}
        - --protected-paths={{ join "," . }}
        {{- end }}
        {{- with .Values.agent.kubepodsCgroup }}
        - --kubepods-cgroup={{ . }}
        {{- end }}
        {{- range .Values.agent.args }}
        - {{ tpl . $ }}
        {{- end }}
        command:
        - /manager
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        image: "{{ .Values.manager.image.repository | default "controller" }}{{- if not (contains "@" (.Values.manager.image.repository | default "controller")) }}:{{ .Values.manager.image.tag | default .Chart.AppVersion }}{{- end }}"
        {{- with .Values.manager.image.pullPolicy }}
        imagePullPolicy: {{ . }}
        {{- end }}
        livenessProbe:
          httpGet:
            path: /healthz
            port: 8081
          initialDelaySeconds: 15
          periodSeconds: 20
        name: agent
        ports:
        - containerPort: 8081
          name: health
          protocol: TCP
        - containerPort: 8443
          name: https
          protocol: TCP
        readinessProbe:
          httpGet:
            path: /readyz
            port: 8081
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          {{- if .Values.agent.resources }}
          {{- toYaml .Values.agent.resources | nindent 10 }}
          {{- else }}
          {}
          {{- end }}
        securityContext:
          {{- if .Values.agent.securityContext }}
          {{- toYaml .Values.agent.securityContext | nindent 10 }}
          {{- else }}
          {}
          {{- end }}
        volumeMounts:
        # C7 (SPEC.md D28): only the kubepods subtree is bind-mounted, not
        # the whole cgroup2 tree (see the volumes.cgroup.hostPath.path
        # comment below and config/agent/agent.yaml's matching comment).
        - mountPath: /host/cgroup/{{ .Values.agent.cgroupHostSubtree }}
          name: cgroup
        # F1 (Fable review): single file, not the whole host /proc
        # directory -- see the volumes.proc.hostPath comment below and
        # config/agent/agent.yaml's matching comment.
        - mountPath: /host/proc/1/mountinfo
          name: proc
          readOnly: true
      securityContext:
        {{- if .Values.agent.podSecurityContext }}
        {{- toYaml .Values.agent.podSecurityContext | nindent 8 }}
        {{- else }}
        {}
        {{- end }}
      serviceAccountName: {{ include "iolimiter-operator.resourceName" (dict "suffix" "agent" "context" $) }}
      {{- if and (hasKey .Values.agent "terminationGracePeriodSeconds") (ne .Values.agent.terminationGracePeriodSeconds nil) }}
      terminationGracePeriodSeconds: {{ .Values.agent.terminationGracePeriodSeconds }}
      {{- end }}
      volumes:
      # D28: the mount source's relative path (under /sys/fs/cgroup) must
      # exactly match the mountPath's suffix above (under /host/cgroup) so
      # D6's discovery, which looks for known kubepods-root subdirectory
      # names under --cgroup-root, still finds it -- nothing else exists
      # under /host/cgroup for it to find. Default matches kind's layout
      # (kubelet.slice/kubelet-kubepods.slice); override
      # agent.cgroupHostSubtree to kubepods.slice (systemd) or
      # kubepods/kubelet/kubepods (cgroupfs) to match a real node's layout.
      - hostPath:
          path: /sys/fs/cgroup/{{ .Values.agent.cgroupHostSubtree }}
          type: Directory
        name: cgroup
      # F1: single file (see the volumeMounts comment above), not a
      # Directory hostPath of the whole host /proc -- a Directory mount let
      # a uid-0 agent jump through /proc/1/root (magic symlink to host PID
      # 1's own rootfs) with only a same-uid ptrace check, no capability
      # required.
      - hostPath:
          path: /proc/1/mountinfo
          type: File
        name: proc
  updateStrategy:
    rollingUpdate:
      maxUnavailable: 10%
    type: RollingUpdate
{{- end }}
EOF

cat > "$AGENT_SA_TEMPLATE" <<'EOF'
{{- if or (not (hasKey .Values.agent "enabled")) (.Values.agent.enabled) }}
apiVersion: v1
kind: ServiceAccount
metadata:
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
  name: {{ include "iolimiter-operator.resourceName" (dict "suffix" "agent" "context" $) }}
  namespace: {{ .Release.Namespace }}
{{- end }}
EOF

# Coordinator review (C6->C7): the plugin templatizes agent-role.yaml,
# agent-rolebinding.yaml and agent-metrics-auth-rolebinding.yaml itself
# (from config/agent/rbac's controller-gen markers) but has no idea
# agent.enabled exists, so agent.enabled=false left them bound to a
# ServiceAccount the chart never created. Wrap each in the same
# agent.enabled guard as agent.yaml/agent-serviceaccount.yaml, in place,
# idempotently (skip if already wrapped by a previous run).
AGENT_GUARD='{{- if or (not (hasKey .Values.agent "enabled")) (.Values.agent.enabled) }}'
for f in "$CHART_DIR/templates/rbac/agent-role.yaml" \
         "$CHART_DIR/templates/rbac/agent-rolebinding.yaml" \
         "$CHART_DIR/templates/rbac/agent-metrics-auth-rolebinding.yaml"; do
  if [[ ! -f "$f" ]]; then
    echo "helm-postprocess: $f not found (did the helm plugin stop generating it?)" >&2
    exit 1
  fi
  if head -n1 "$f" | grep -qF "$AGENT_GUARD"; then
    continue
  fi
  { printf '%s\n' "$AGENT_GUARD"; cat "$f"; printf '{{- end }}\n'; } > "$f.tmp"
  mv "$f.tmp" "$f"
done

# C7 (SPEC.md D27): ValidatingAdmissionPolicy + binding pair, mirroring
# config/vap. Templated (not a literal SA username, unlike config/vap's
# kustomize version): the release namespace and SA names are only known at
# install time here.
cat > "$CHART_DIR/templates/rbac/podiolimit-admission-policy.yaml" <<'EOF'
{{- if or (not (hasKey .Values.admissionPolicy "enabled")) (.Values.admissionPolicy.enabled) }}
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: {{ include "iolimiter-operator.resourceName" (dict "suffix" "podiolimit-write-restricted" "context" $) }}
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["storage.maurice.fr"]
        apiVersions: ["*"]
        operations: ["CREATE", "UPDATE"]
        resources: ["podiolimits"]
  validations:
    - expression: >-
        request.userInfo.username ==
        'system:serviceaccount:{{ .Release.Namespace }}:{{ include "iolimiter-operator.serviceAccountName" . }}' ||
        (request.operation == 'UPDATE' && object.spec == oldObject.spec)
      message: >-
        podiolimits (the main resource) may only be created, or have its
        spec changed, by the iolimiter-operator controller service account; a
        metadata-only update (e.g. stripping a stuck finalizer during
        uninstall) is allowed from anyone RBAC permits (SPEC.md D27, D33).
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: {{ include "iolimiter-operator.resourceName" (dict "suffix" "podiolimit-write-restricted-binding" "context" $) }}
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
spec:
  policyName: {{ include "iolimiter-operator.resourceName" (dict "suffix" "podiolimit-write-restricted" "context" $) }}
  validationActions: ["Deny"]
  matchResources: {}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: {{ include "iolimiter-operator.resourceName" (dict "suffix" "podiolimit-status-write-restricted" "context" $) }}
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: ["storage.maurice.fr"]
        apiVersions: ["*"]
        operations: ["UPDATE"]
        resources: ["podiolimits/status"]
  validations:
    - expression: >-
        request.userInfo.username ==
        'system:serviceaccount:{{ .Release.Namespace }}:{{ include "iolimiter-operator.resourceName" (dict "suffix" "agent" "context" $) }}'
      message: >-
        podiolimits/status may only be updated by the iolimiter-operator
        agent service account (SPEC.md D27).
    - expression: >-
        has(request.userInfo.extra) &&
        'authentication.kubernetes.io/node-name' in request.userInfo.extra &&
        request.userInfo.extra['authentication.kubernetes.io/node-name'][0] == object.spec.nodeName
      message: >-
        podiolimits/status may only be updated by the agent running on the
        PodIOLimit's own spec.nodeName (SPEC.md D27, D39, OQ2).
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: {{ include "iolimiter-operator.resourceName" (dict "suffix" "podiolimit-status-write-restricted-binding" "context" $) }}
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
spec:
  policyName: {{ include "iolimiter-operator.resourceName" (dict "suffix" "podiolimit-status-write-restricted" "context" $) }}
  validationActions: ["Deny"]
  matchResources: {}
{{- end }}
EOF

# D29 (Helm PSA gap): optional chart-managed Namespace, labeled with the
# same PSA "privileged" labels config/default's kustomize
# namespace_psa_patch.yaml adds (D14: the agent's hostPath volumes need
# them). Off by default (namespace.create: false) to keep the existing
# `helm install --create-namespace` flow working unmodified; a fresh
# PSA-enforcing-cluster install should set namespace.create=true instead of
# passing --create-namespace (the two are mutually exclusive: Helm can't
# adopt a namespace `--create-namespace` already created without this
# template's own ownership metadata).
#
# Not plugin-native, checked (not guessed): the kubebuilder helm/v2-alpha
# plugin drops config/manager/manager.yaml's Namespace object entirely --
# verified directly by running `kubebuilder edit --plugins=helm/v2-alpha`
# alone (no postprocess) and finding no namespace template anywhere in
# dist/chart, including moving the PSA labels onto the Namespace object
# itself (inline, replacing the JSON6902 patch) to see if that changed the
# plugin's behaviour; it didn't -- the plugin never emits a Namespace
# template regardless of that object's labels. So this has to be
# postprocess-authored; there is no plugin-native path to prefer here.
cat > "$CHART_DIR/templates/namespace.yaml" <<'EOF'
{{- if .Values.namespace.create }}
apiVersion: v1
kind: Namespace
metadata:
  name: {{ .Release.Namespace }}
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    app.kubernetes.io/name: {{ include "iolimiter-operator.name" . }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
    app.kubernetes.io/instance: {{ .Release.Name }}
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
{{- end }}
EOF

# F2 (Fable review): the kubebuilder plugin regenerates its own NOTES.txt
# from scratch on every `make helm-chart` (kubebuilder edit --force), so a
# hand-edit here would be silently lost -- rewrite it from a heredoc like
# agent.yaml above. Names agent.cgroupHostSubtree on the one thing Helm
# actually prints after install, since a wrong value fails every agent pod
# with no signal on the IOLimiter/PodIOLimit status (F2's whole point).
cat > "$CHART_DIR/templates/NOTES.txt" <<'EOF'
Thank you for installing {{ .Chart.Name }}.

Your release is named {{ .Release.Name }}.

The controller and CRDs have been installed in namespace {{ .Release.Namespace }}.

IMPORTANT: the agent DaemonSet's cgroup hostPath must match this node's
layout, or every agent pod gets stuck ContainerCreating with no signal on
the IOLimiter/PodIOLimit status (only `kubectl describe pod` shows it):

  agent.cgroupHostSubtree = {{ .Values.agent.cgroupHostSubtree }}

Typical systemd node (this chart's default): kubepods.slice
kind: kubelet.slice/kubelet-kubepods.slice (see hack/kind-values.yaml)
cgroupfs: kubepods, or kubelet/kubepods under kubelet.slice

To verify the installation:

  kubectl get pods -n {{ .Release.Namespace }}
  kubectl get customresourcedefinitions

To learn more about the release, try:

  $ helm status {{ .Release.Name }} -n {{ .Release.Namespace }}
  $ helm get all {{ .Release.Name }} -n {{ .Release.Namespace }}
EOF

if grep -q '^agent:' "$VALUES_FILE"; then
  echo "helm-postprocess: values.yaml already has an agent: block, leaving it as-is (edit it by hand if the agent.yaml template above changed)." >&2
else
  cat >> "$VALUES_FILE" <<'EOF'

## Configure the node agent DaemonSet (SPEC.md §6.1/§8).
##
agent:
  ## Set to false to skip agent installation.
  ##
  enabled: true

  ## Extra agent flags, appended after the built-in ones below. Use this to
  ## set --zap-log-level=debug (or =error, or an integer), --zap-encoder,
  ## --zap-devel, etc.
  ##
  args: []

  ## --resync-period: how often the agent re-reads io.max to detect drift
  ## (SPEC.md D11).
  ##
  resyncPeriod: 60s

  ## --allow-root-device: opt in to throttling any device backing
  ## agent.protectedPaths below (SPEC.md D23/D34), not just the root disk.
  ## Off by default: throttling one of those devices also throttles every
  ## pod's rootfs/writable layer on that node and can cause node-wide
  ## priority inversion under ext4 data=ordered.
  ##
  allowRootDevice: false

  ## --protected-paths: host paths whose whole-disk device is refused
  ## unless allowRootDevice is set (SPEC.md D34). Missing paths are
  ## ignored. Default matches the agent's own --protected-paths default:
  ## node root, kubelet's directory, and the container runtime's own
  ## storage directories (their writable layers share the same
  ## fsync-inversion risk as the root disk, K7).
  ##
  protectedPaths:
    - /
    - /var/lib/kubelet
    - /var/lib/containerd
    - /var/lib/containers

  ## --kubepods-cgroup: override kubepods root discovery (SPEC.md D6). Leave
  ## empty to auto-discover.
  ##
  kubepodsCgroup: ""

  ## Host path (relative to /sys/fs/cgroup) bind-mounted into the agent as
  ## its cgroup root (SPEC.md D28: narrowed to the kubepods subtree, not the
  ## whole cgroup2 tree). Must match the node's actual layout:
  ##   - typical systemd node (default): kubepods.slice
  ##   - kind:                        kubelet.slice/kubelet-kubepods.slice
  ##     (see hack/kind-values.yaml -- `make deploy-kind`'s kustomize path
  ##     uses hack/kind-overlay for the same override)
  ##   - cgroupfs, kubelet default:  kubepods
  ##   - cgroupfs, under kubelet/:   kubelet/kubepods
  ## A wrong value fails every agent pod at ContainerCreating (the hostPath
  ## type is Directory, so kubelet refuses to start the pod if the path
  ## doesn't exist) -- `kubectl describe pod` on a stuck agent pod is the
  ## only signal, there is nothing in the operator's own status (Fable
  ## review F2).
  ##
  cgroupHostSubtree: kubepods.slice

  ## Pod-level security settings (SPEC.md D14: not privileged).
  ##
  podSecurityContext:
    runAsUser: 0
    runAsNonRoot: false
    seccompProfile:
      type: RuntimeDefault

  ## Container-level security settings (SPEC.md D14).
  ##
  securityContext:
    privileged: false
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop:
        - ALL

  ## Resource limits and requests.
  ##
  resources:
    requests:
      cpu: 10m
      memory: 32Mi
    limits:
      cpu: 200m
      memory: 128Mi

  ## Agent pod's tolerations. Defaults to tolerating everything: the agent
  ## must run on every node with pods it may need to throttle.
  ##
  tolerations:
    - operator: Exists

  ## Agent pod's node selector.
  ##
  nodeSelector: {}

  ## Priority class name.
  ##
  priorityClassName: system-node-critical

  ## Termination grace period seconds.
  ##
  terminationGracePeriodSeconds: 10

## ValidatingAdmissionPolicy hardening (SPEC.md C7/D27): only the controller
## SA may write podiolimits (the main resource), and only the agent SA may
## write podiolimits/status, and only for its own node. Requires Kubernetes
## 1.32+ (already this chart's minimum, OQ2) with
## admissionregistration.k8s.io/v1 ValidatingAdmissionPolicy enabled
## (default-on in-tree since 1.30). Disable if your cluster doesn't support
## it.
##
admissionPolicy:
  enabled: true

## Release namespace management (SPEC.md D29). The kubebuilder helm plugin
## never templates a Namespace object (namespace lifecycle is left to the
## installer, e.g. `helm install --create-namespace`), which means a plain
## `--create-namespace` namespace carries none of the PSA "privileged"
## labels the agent's hostPath volumes need (D14). Set namespace.create:
## true to let the chart manage the namespace (with those labels) itself --
## do NOT also pass --create-namespace in that case, and don't enable this
## against a namespace an earlier release already owns outside Helm.
## Leave false (default) to keep the previous behaviour: create/label the
## namespace yourself first (see the README's Helm install section).
##
namespace:
  create: false
EOF
fi

# The plugin re-scaffolds its own unpinned test-chart.yml workflow on every
# run; ci.yml already covers helm-lint + a real e2e smoke test against the
# pinned kind cluster (SPEC.md C6), so drop the generated one.
rm -f .github/workflows/test-chart.yml

# The plugin also auto-generates its own "extras" copy of config/vap's
# ValidatingAdmissionPolicy/Binding objects (like it does for the agent
# DaemonSet, which this script overwrites above): a near-literal copy of
# the kustomize-rendered YAML, with two problems -- it doesn't gate on
# admissionPolicy.enabled, and (worse) it invents an invalid
# `namespace: {{ .Release.Namespace }}` on a cluster-scoped
# ValidatingAdmissionPolicy/Binding (would be rejected by the apiserver).
# The correct, templated, admissionPolicy.enabled-gated versions this
# script writes to templates/rbac/podiolimit-admission-policy.yaml above
# replace them; drop the plugin's own copies so they aren't applied twice.
rm -f "$CHART_DIR"/templates/extras/podiolimit-*.yaml

echo "helm-postprocess: agent DaemonSet + ServiceAccount templatized, values.yaml agent: block ensured."
