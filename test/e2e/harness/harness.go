//go:build e2e

/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package harness holds everything test/e2e/agent and test/e2e/operator
// share: the pinned-context clients, kind topology, builders for local
// PV/PVC/pod/StatefulSet/IOLimiter/PodIOLimit, and the docker-exec/kubectl
// escape hatches for node-level assertions (SPEC.md §10, §11 C3/C5).
//
// C3 (test/e2e/agent) plays the controller by hand against a real kernel
// with the controller-manager Deployment scaled to 0. C5
// (test/e2e/operator) runs the real controller and drives everything
// through IOLimiter. Both packages call Init once from their own TestMain
// and then use this package's exported clients/helpers; neither package
// may build its own client or touch the ambient kubeconfig context.
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/internal/iomax"
)

// Fixed identifiers of the deployed manager (config/agent, config/manager,
// config/default) and of the kind topology (hack/kind-config.yaml,
// hack/setup-loopdev.sh).
const (
	AgentNamespace          = "iolimiter-operator-system"
	AgentDaemonSet          = "iolimiter-operator-agent"
	AgentLabelSelector      = "control-plane=agent"
	ControllerDeployment    = "iolimiter-operator-controller-manager"
	ControllerLabelSelector = "control-plane=controller-manager"
	DefaultKindCluster      = "blkio-limiter"
	StorageClassName        = "blkio-local"

	// FinalizerName mirrors the unexported constant of the same name in
	// internal/controller/finalizer.go (D9). The agent package plays the
	// controller by hand, so it must add/remove the exact same finalizer.
	FinalizerName = "storage.maurice.fr/io-max-reset"

	// PollInterval is how often status-polling helpers re-check.
	PollInterval = 2 * time.Second

	// hostnameLabelKey pins a pod/DaemonSet to a specific node.
	hostnameLabelKey = "kubernetes.io/hostname"
	// appLabelKey is the "app" pod label this package's builders use throughout.
	appLabelKey = "app"
	// kubectlGet is the kubectl verb DumpDiagnostics repeats across resource kinds.
	kubectlGet = "get"
)

// Package-level clients and topology, built once by Init against the
// pinned kind context. Callers must use these, never build their own.
var (
	KindCluster string
	WorkerNode  string
	RestCfg     *rest.Config
	Clientset   kubernetes.Interface
	K8sClient   client.Client

	// ControllerClient is K8sClient, but impersonating the real controller
	// ServiceAccount (SPEC.md C7/D27: only that identity may create,
	// update or delete a PodIOLimit's main resource under the VAP). Only
	// test/e2e/agent (which "plays the controller" by hand, §11 C3) needs
	// it, via CreatePodIOLimit/DeletePodIOLimit below; every read (Get,
	// List, watch) still goes through the plain K8sClient, since the VAP
	// only restricts writes.
	ControllerClient client.Client
)

// controllerSAUsername mirrors config/vap's own hardcoded assumption
// (SPEC.md D27's comment there): ControllerDeployment is also this
// project's controller ServiceAccount name by convention, verified
// directly against the live cluster in test/e2e/hardening.
func controllerSAUsername() string {
	return "system:serviceaccount:" + AgentNamespace + ":" + ControllerDeployment
}

// Init builds the pinned-context clients (context kind-$KIND_CLUSTER,
// default DefaultKindCluster). Every kubectl/client-go call in both e2e
// packages goes through the clients this sets; never the ambient context.
func Init() error {
	KindCluster = os.Getenv("KIND_CLUSTER")
	if KindCluster == "" {
		KindCluster = DefaultKindCluster
	}
	WorkerNode = KindCluster + "-worker"

	cfg, err := buildRESTConfig(KindCluster)
	if err != nil {
		return fmt.Errorf("building kubeconfig for context kind-%s: %w", KindCluster, err)
	}
	RestCfg = cfg

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("building clientset: %w", err)
	}
	Clientset = cs

	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		return fmt.Errorf("registering client-go scheme: %w", err)
	}
	if err := storagev1alpha1.AddToScheme(sch); err != nil {
		return fmt.Errorf("registering storage.maurice.fr scheme: %w", err)
	}
	// Not in clientgoscheme (it's the apiextensions apiserver's own group,
	// not client-go's built-in set): needed for
	// TestVAPPolicies_MatchAllServedVersions to read the live CRD's served
	// versions.
	if err := apiextensionsv1.AddToScheme(sch); err != nil {
		return fmt.Errorf("registering apiextensions.k8s.io scheme: %w", err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		return fmt.Errorf("building controller-runtime client: %w", err)
	}
	K8sClient = cl

	controllerCfg := rest.CopyConfig(cfg)
	controllerCfg.Impersonate = rest.ImpersonationConfig{UserName: controllerSAUsername()}
	controllerCl, err := client.New(controllerCfg, client.Options{Scheme: sch})
	if err != nil {
		return fmt.Errorf("building controller-impersonating controller-runtime client: %w", err)
	}
	ControllerClient = controllerCl

	return nil
}

// buildRESTConfig loads the ambient kubeconfig but forces the context to
// kind-<cluster>, exactly like the Makefile's KCTL wrapper.
func buildRESTConfig(cluster string) (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: "kind-" + cluster}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
}

// WaitDaemonSetReady polls a DaemonSet until every scheduled replica is
// ready.
func WaitDaemonSetReady(ctx context.Context, ns, name string) error {
	for {
		ds, err := Clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && ds.Status.DesiredNumberScheduled > 0 &&
			ds.Status.NumberReady == ds.Status.DesiredNumberScheduled &&
			ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemonset %s/%s not ready: %w", ns, name, ctx.Err())
		case <-time.After(PollInterval):
		}
	}
}

// CheckLoopMounts verifies the worker's two e2e loop devices
// (hack/setup-loopdev.sh) are mounted before any test tries to build a
// local PV on top of them.
func CheckLoopMounts() error {
	for _, disk := range []string{"disk0", "disk1"} {
		mnt := "/mnt/blkio-e2e/" + disk
		out, err := exec.Command("docker", "exec", WorkerNode, "findmnt", "-no", "SOURCE", mnt).CombinedOutput() //nolint:gosec // fixed binary, fixed args.
		if err != nil || strings.TrimSpace(string(out)) == "" {
			return fmt.Errorf("%s not mounted on %s (findmnt: %v, output: %s)", mnt, WorkerNode, err, out)
		}
	}
	return nil
}

// SelfHealLeftoverNamespaces deletes any e2e-* namespace left behind by a
// previous run that was killed before its t.Cleanup could run, so a killed
// run can never poison the next one.
func SelfHealLeftoverNamespaces(ctx context.Context) error {
	list, err := Clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, ns := range list.Items {
		if !strings.HasPrefix(ns.Name, "e2e-") || ns.Status.Phase == corev1.NamespaceTerminating {
			continue
		}
		fmt.Fprintf(os.Stderr, "e2e: WARNING: leftover namespace %q from a previous killed run; deleting it before running any test\n", ns.Name)
		if delErr := Clientset.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("deleting leftover namespace %q: %w", ns.Name, delErr)
		}
	}
	return nil
}

// ScaleControllerManager scales the controller-manager Deployment to
// replicas and waits until its observed replica count matches, so a
// suite that needs the controller off (agent) or on (operator) never
// races the Deployment controller's own reconcile.
func ScaleControllerManager(ctx context.Context, replicas int32) error {
	dep, err := Clientset.AppsV1().Deployments(AgentNamespace).Get(ctx, ControllerDeployment, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting deployment %s/%s: %w", AgentNamespace, ControllerDeployment, err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != replicas {
		dep.Spec.Replicas = new(replicas)
		if _, err := Clientset.AppsV1().Deployments(AgentNamespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("scaling deployment %s/%s to %d: %w", AgentNamespace, ControllerDeployment, replicas, err)
		}
	}
	for {
		d, err := Clientset.AppsV1().Deployments(AgentNamespace).Get(ctx, ControllerDeployment, metav1.GetOptions{})
		if err == nil && d.Status.Replicas == replicas && d.Status.ReadyReplicas == replicas {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for deployment %s/%s to reach %d replicas: %w", AgentNamespace, ControllerDeployment, replicas, ctx.Err())
		case <-time.After(PollInterval):
		}
	}
}

// controlPlaneNode is the kind control-plane node name for KindCluster,
// used by ExcludeWorkerFromAgentDaemonSet as the "somewhere that isn't the
// worker" pin.
func controlPlaneNode() string { return KindCluster + "-control-plane" }

// ExcludeWorkerFromAgentDaemonSet patches the agent DaemonSet's
// nodeSelector to pin it onto the control-plane node only, which evicts
// the worker's agent pod (simulating "the agent is down" for
// TestDeleteWhileAgentDown) without touching any other manifest.
func ExcludeWorkerFromAgentDaemonSet(ctx context.Context) error {
	ds, err := Clientset.AppsV1().DaemonSets(AgentNamespace).Get(ctx, AgentDaemonSet, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting daemonset %s/%s: %w", AgentNamespace, AgentDaemonSet, err)
	}
	ds.Spec.Template.Spec.NodeSelector = map[string]string{hostnameLabelKey: controlPlaneNode()}
	if _, err := Clientset.AppsV1().DaemonSets(AgentNamespace).Update(ctx, ds, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("patching daemonset %s/%s nodeSelector: %w", AgentNamespace, AgentDaemonSet, err)
	}
	for {
		pods, err := Clientset.CoreV1().Pods(AgentNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: AgentLabelSelector,
			FieldSelector: "spec.nodeName=" + WorkerNode,
		})
		if err == nil && len(pods.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for agent pod to leave %s: %w", WorkerNode, ctx.Err())
		case <-time.After(PollInterval):
		}
	}
}

// RestoreAgentDaemonSet clears any nodeSelector patch
// ExcludeWorkerFromAgentDaemonSet left and waits for the DaemonSet to be
// ready again on every node (including the worker). Safe to call when
// there is nothing to restore (idempotent), so it doubles as a TestMain
// self-heal and a t.Cleanup belt-and-braces restore.
func RestoreAgentDaemonSet(ctx context.Context) error {
	ds, err := Clientset.AppsV1().DaemonSets(AgentNamespace).Get(ctx, AgentDaemonSet, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting daemonset %s/%s: %w", AgentNamespace, AgentDaemonSet, err)
	}
	if len(ds.Spec.Template.Spec.NodeSelector) > 0 {
		ds.Spec.Template.Spec.NodeSelector = nil
		if _, err := Clientset.AppsV1().DaemonSets(AgentNamespace).Update(ctx, ds, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("restoring daemonset %s/%s nodeSelector: %w", AgentNamespace, AgentDaemonSet, err)
		}
	}
	return WaitDaemonSetReady(ctx, AgentNamespace, AgentDaemonSet)
}

// KubectlOutput runs kubectl pinned to the test context and returns
// combined output. Used only for exec (no controller-runtime equivalent)
// and for diagnostics dumps.
func KubectlOutput(args ...string) (string, error) {
	full := append([]string{"--context", "kind-" + KindCluster}, args...)
	out, err := exec.Command("kubectl", full...).CombinedOutput() //nolint:gosec // fixed binary, test-controlled args.
	return string(out), err
}

// NewTestNamespace creates a uniquely named namespace and registers its
// cleanup. Failed tests keep their namespace (logged) for debugging.
func NewTestNamespace(t *testing.T, prefix string) string {
	t.Helper()
	name := fmt.Sprintf("e2e-%s-%s", prefix, randSuffix())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, K8sClient.Create(context.Background(), ns), "creating namespace %s", name)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("test failed: keeping namespace %q for debugging", name)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := K8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil &&
			!apierrors.IsNotFound(err) {
			t.Logf("cleanup: deleting namespace %q: %v", name, err)
		}
	})
	return name
}

func randSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// PollUntil polls check every PollInterval until it reports done, logging
// its status string whenever it changes so CI logs show progress instead
// of silence. Fails the test on timeout.
func PollUntil(t *testing.T, timeout time.Duration, what string, check func() (done bool, status string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		done, status := check()
		if status != "" && status != last {
			t.Logf("%s: %s", what, status)
			last = status
		}
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out after %s (last status: %q)", what, timeout, last)
		}
		time.Sleep(PollInterval)
	}
}

// DumpDiagnostics prints everything useful to debug a stuck/failed test:
// the PodIOLimits, IOLimiters, events, pods, the agent+controller logs
// grepped for this namespace, and the node's io.max for cgroupPath if
// given.
func DumpDiagnostics(t *testing.T, ns, cgroupPath string) {
	t.Helper()
	t.Logf("=== diagnostics for namespace %s ===", ns)
	for _, args := range [][]string{
		{kubectlGet, "podiolimits", "-n", ns, "-o", "yaml"},
		{kubectlGet, "iolimiters", "-n", ns, "-o", "yaml"},
		{kubectlGet, "events", "-n", ns, "--sort-by=.lastTimestamp"},
		{kubectlGet, "pods", "-n", ns, "-o", "wide"},
	} {
		out, err := KubectlOutput(args...)
		if err != nil {
			t.Logf("kubectl %s: %v", strings.Join(args, " "), err)
			continue
		}
		t.Logf("kubectl %s:\n%s", strings.Join(args, " "), out)
	}
	if out, err := AgentPodLogs(300); err == nil {
		t.Logf("agent logs (grep %s):\n%s", ns, GrepLines(out, ns))
	}
	if out, err := ControllerPodLogs(300); err == nil {
		t.Logf("controller logs (grep %s):\n%s", ns, GrepLines(out, ns))
	}
	if cgroupPath != "" {
		if content, err := NodeIOMaxRaw(cgroupPath); err == nil {
			t.Logf("node io.max at %s:\n%s", cgroupPath, content)
		}
	}
}

// AgentPodLogs returns the last tailLines of every agent DaemonSet pod's
// logs, each line prefixed with its source pod. `kubectl logs ds/<name>`
// picks exactly one of the (here, two: control-plane + worker) matching
// pods arbitrarily, which silently drops the worker's own log half the
// time; `-l <selector>` streams every matching pod.
func AgentPodLogs(tailLines int) (string, error) {
	return KubectlOutput("logs", "-n", AgentNamespace, "-l", AgentLabelSelector, "--prefix", fmt.Sprintf("--tail=%d", tailLines))
}

// ControllerPodLogs returns the last tailLines of the controller-manager
// pod's logs.
func ControllerPodLogs(tailLines int) (string, error) {
	return KubectlOutput("logs", "-n", AgentNamespace, "-l", ControllerLabelSelector, "--prefix", fmt.Sprintf("--tail=%d", tailLines))
}

// GrepLines returns every line of s containing needle.
func GrepLines(s, needle string) string {
	var kept []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.Contains(l, needle) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// NodeIOMaxRaw reads the raw io.max content of cgroupPath (relative to the
// cgroup2 root, i.e. PodIOLimit.status.cgroupPath) directly from the
// worker node via docker exec: this can't be read from inside any test
// pod (its own cgroupns shows its container cgroup, not the pod's), so
// the suite reaches straight into the node the way the acceptance
// criteria requires.
func NodeIOMaxRaw(cgroupPath string) (string, error) {
	path := "/sys/fs/cgroup/" + strings.TrimPrefix(cgroupPath, "/") + "/io.max"
	out, err := exec.Command("docker", "exec", WorkerNode, "cat", path).CombinedOutput() //nolint:gosec // fixed binary, test-controlled args.
	if err != nil {
		return "", fmt.Errorf("docker exec %s cat %s: %w (output: %s)", WorkerNode, path, err, out)
	}
	return string(out), nil
}

// NodeIOMax reads and parses the node's io.max for cgroupPath into a
// device -> line map ("MAJ:MIN rbps=... wbps=... riops=... wiops=...").
func NodeIOMax(t *testing.T, cgroupPath string) map[string]string {
	t.Helper()
	content, err := NodeIOMaxRaw(cgroupPath)
	require.NoErrorf(t, err, "reading node io.max for cgroup %s", cgroupPath)
	lines := map[string]string{}
	for l := range strings.SplitSeq(strings.TrimSpace(content), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		dev := strings.Fields(l)[0]
		lines[dev] = l
	}
	return lines
}

// ExecPod runs a shell script inside a container via kubectl exec.
func ExecPod(ns, pod, container, script string) (string, error) {
	return KubectlOutput("exec", "-n", ns, pod, "-c", container, "--", "sh", "-c", script)
}

// RestartAgentDaemonSet rolls out a restart of the agent DaemonSet and
// waits for it to complete. `kubectl rollout status` (not a hand-rolled
// poll on Ready counts) tracks the DaemonSet's own updateRevision, so it
// can't return early on stale status fields the way polling
// NumberReady/UpdatedNumberScheduled right after the restart patch
// (before the DaemonSet controller has reacted to it) would.
func RestartAgentDaemonSet(t *testing.T) {
	t.Helper()
	out, err := KubectlOutput("-n", AgentNamespace, "rollout", "restart", "ds/"+AgentDaemonSet)
	require.NoErrorf(t, err, "rollout restart ds/%s: %s", AgentDaemonSet, out)
	out, err = KubectlOutput("-n", AgentNamespace, "rollout", "status", "ds/"+AgentDaemonSet, "--timeout=2m")
	require.NoErrorf(t, err, "rollout status ds/%s: %s", AgentDaemonSet, out)
}

// diskMount returns the worker-side mount point of one of the two e2e
// loop devices provisioned by hack/setup-loopdev.sh ("disk0" or "disk1").
func diskMount(disk string) string {
	return "/mnt/blkio-e2e/" + disk
}

// MkTestDir creates <diskMount(disk)>/<name> on the worker node via
// docker exec, for a static local PV to point at (§10: "each test creates
// its own directory").
func MkTestDir(t *testing.T, disk, name string) string {
	t.Helper()
	dir := diskMount(disk) + "/" + name
	out, err := exec.Command("docker", "exec", WorkerNode, "mkdir", "-p", dir).CombinedOutput() //nolint:gosec // fixed binary, test-controlled args.
	require.NoErrorf(t, err, "mkdir %s on %s: %s", dir, WorkerNode, out)
	return dir
}

// NewLocalPV builds a static local PV on disk (worker node affinity). It
// is left Available (unbound): a PVC that names it via spec.volumeName
// binds explicitly (NewLocalPVC), while a PVC that merely matches its
// storageClassName/size/accessModes (e.g. a StatefulSet's
// volumeClaimTemplate) binds through the ordinary static-provisioning
// control loop.
func NewLocalPV(name, path string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("64Mi")},
			VolumeMode:                    ptr.To(corev1.PersistentVolumeFilesystem),
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			StorageClassName:              StorageClassName,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				Local: &corev1.LocalVolumeSource{Path: path},
			},
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key: hostnameLabelKey, Operator: corev1.NodeSelectorOpIn, Values: []string{WorkerNode},
						}},
					}},
				},
			},
		},
	}
}

// NewLocalPVC builds a PVC pre-bound to pvName via spec.volumeName (§10).
func NewLocalPVC(ns, name, pvName string) *corev1.PersistentVolumeClaim {
	sc := StorageClassName
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeName:       pvName,
			StorageClassName: &sc,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("64Mi")},
			},
		},
	}
}

// VolMount is one pod volume: its pod-volume name and the PVC backing it.
type VolMount struct {
	Name string
	PVC  string
}

// NewDataPod builds a Burstable-QoS busybox pod on the worker, sleeping
// forever, with one mount per VolMount at /data/<name>. imagePullPolicy is
// IfNotPresent: busybox:1.37 is pre-pulled into every node by
// `make kind-setup` (kind-load-busybox), so no test pulls from a registry.
func NewDataPod(ns, name string, volumes []VolMount, labels map[string]string) *corev1.Pod {
	vols := make([]corev1.Volume, 0, len(volumes))
	mounts := make([]corev1.VolumeMount, 0, len(volumes))
	for _, v := range volumes {
		vols = append(vols, corev1.Volume{
			Name: v.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: v.PVC},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: v.Name, MountPath: "/data/" + v.Name})
	}
	allLabels := map[string]string{appLabelKey: name}
	maps.Copy(allLabels, labels)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: allLabels},
		Spec:       dataPodSpec(vols, mounts),
	}
}

// NewEmptyDirPod builds the same shape of pod as NewDataPod, but with a
// single emptyDir volume: used by TestUnsupportedVolumeReported (an
// emptyDir has no block device, so it can never be resolved (§6.1 step 4,
// NoBlockDevice)).
func NewEmptyDirPod(ns, name, volName string, labels map[string]string) *corev1.Pod {
	vols := []corev1.Volume{{Name: volName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	mounts := []corev1.VolumeMount{{Name: volName, MountPath: "/data/" + volName}}
	allLabels := map[string]string{appLabelKey: name}
	maps.Copy(allLabels, labels)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: allLabels},
		Spec:       dataPodSpec(vols, mounts),
	}
}

func dataPodSpec(vols []corev1.Volume, mounts []corev1.VolumeMount) corev1.PodSpec {
	return corev1.PodSpec{
		NodeSelector: map[string]string{hostnameLabelKey: WorkerNode},
		Containers: []corev1.Container{{
			Name:            appLabelKey,
			Image:           "busybox:1.37",
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", "sleep infinity"},
			VolumeMounts:    mounts,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				},
			},
		}},
		Volumes: vols,
	}
}

// NewStatefulSet builds a 1-replica StatefulSet whose sole container mounts
// one PVC (from volumeClaimTemplateName, provisioned via StorageClassName)
// at /data, used by TestIOLimiterEndToEnd (§11 C5).
func NewStatefulSet(ns, name, volumeClaimTemplateName string, podLabels map[string]string) *appsv1.StatefulSet {
	allLabels := map[string]string{appLabelKey: name}
	maps.Copy(allLabels, podLabels)
	sc := StorageClassName
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: name,
			Replicas:    new(int32(1)),
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{appLabelKey: name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: allLabels},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{hostnameLabelKey: WorkerNode},
					Containers: []corev1.Container{{
						Name:            appLabelKey,
						Image:           "busybox:1.37",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"sh", "-c", "sleep infinity"},
						VolumeMounts:    []corev1.VolumeMount{{Name: volumeClaimTemplateName, MountPath: "/data"}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("16Mi"),
							},
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: volumeClaimTemplateName},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: &sc,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("64Mi")},
					},
				},
			}},
		},
	}
}

// StatefulSetPodName is the name of a StatefulSet's ordinal-0 pod.
func StatefulSetPodName(stsName string) string { return stsName + "-0" }

// WaitPodRunning polls until the pod's phase is Running and its Ready
// condition is true (so the PVC is mounted and the container started,
// which is what makes the pod cgroup and its io.max file exist).
func WaitPodRunning(t *testing.T, ctx context.Context, ns, name string, timeout time.Duration) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	PollUntil(t, timeout, fmt.Sprintf("pod %s/%s running", ns, name), func() (bool, string) {
		if err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod); err != nil {
			return false, fmt.Sprintf("get error: %v", err)
		}
		ready := podReadyE2E(&pod)
		return ready, fmt.Sprintf("phase=%s ready=%v", pod.Status.Phase, ready)
	})
	return &pod
}

// WaitPodGone polls until the pod no longer exists.
func WaitPodGone(t *testing.T, ctx context.Context, ns, name string, timeout time.Duration) {
	t.Helper()
	PollUntil(t, timeout, fmt.Sprintf("pod %s/%s gone", ns, name), func() (bool, string) {
		var pod corev1.Pod
		err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod)
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		if err != nil {
			return false, err.Error()
		}
		return false, fmt.Sprintf("phase=%s uid=%s", pod.Status.Phase, pod.UID)
	})
}

// WaitPodRecreated polls until name in ns exists, is Running/Ready, and has
// a UID different from oldUID. Deliberately does not require observing a
// NotFound gap in between: a StatefulSet controller can recreate a pod
// fast enough that a poll loop never actually samples the moment it's
// gone, only the old UID and then the new one.
func WaitPodRecreated(t *testing.T, ctx context.Context, ns, name string, oldUID types.UID, timeout time.Duration) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	PollUntil(t, timeout, fmt.Sprintf("pod %s/%s recreated with a new UID", ns, name), func() (bool, string) {
		if err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod); err != nil {
			return false, fmt.Sprintf("get error: %v", err)
		}
		if pod.UID == oldUID {
			return false, "still the old UID: " + string(pod.UID)
		}
		ready := podReadyE2E(&pod)
		return ready, fmt.Sprintf("uid=%s phase=%s ready=%v", pod.UID, pod.Status.Phase, ready)
	})
	return &pod
}

func podReadyE2E(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodIOLimitName mirrors D18: <podName>-<podUID[:8]>.
func PodIOLimitName(podName string, podUID types.UID) string {
	uid := string(podUID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return podName + "-" + uid
}

// NewPodIOLimit builds a PodIOLimit exactly as the controller would (D18),
// but by hand: test/e2e/agent plays the controller (§11 C3), including
// adding the deletion-handshake finalizer (D9) itself. Not used by
// test/e2e/operator, which drives everything through IOLimiter instead.
func NewPodIOLimit(pod *corev1.Pod, volumes []storagev1alpha1.PodVolumeLimit) *storagev1alpha1.PodIOLimit {
	return &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{
			Name:       PodIOLimitName(pod.Name, pod.UID),
			Namespace:  pod.Namespace,
			Finalizers: []string{FinalizerName},
		},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: pod.Spec.NodeName,
			PodName:  pod.Name,
			PodUID:   pod.UID,
			QOSClass: pod.Status.QOSClass,
			Volumes:  volumes,
		},
	}
}

// NewIOLimiter builds an IOLimiter selecting pods by podLabels, applying
// limits to the named volumes.
func NewIOLimiter(ns, name string, podLabels map[string]string, volumes []storagev1alpha1.VolumeLimit) *storagev1alpha1.IOLimiter {
	return &storagev1alpha1.IOLimiter{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: storagev1alpha1.IOLimiterSpec{
			PodSelector: metav1.LabelSelector{MatchLabels: podLabels},
			Volumes:     volumes,
		},
	}
}

// VolumeLimit builds one IOLimiter volume entry from optional bytes/s and
// iops values (0 = unset = max).
func VolumeLimit(name string, readBPS, writeBPS, readIOPS, writeIOPS int64) storagev1alpha1.VolumeLimit {
	var l storagev1alpha1.IOLimits
	if readBPS > 0 {
		l.ReadBytesPerSecond = new(resource.MustParse(strconv.FormatInt(readBPS, 10)))
	}
	if writeBPS > 0 {
		l.WriteBytesPerSecond = new(resource.MustParse(strconv.FormatInt(writeBPS, 10)))
	}
	if readIOPS > 0 {
		l.ReadIOPS = new(readIOPS)
	}
	if writeIOPS > 0 {
		l.WriteIOPS = new(writeIOPS)
	}
	return storagev1alpha1.VolumeLimit{Name: name, Limits: l}
}

// DeviceLimits builds DeviceLimits from optional bytes/s and iops values (0
// = unset = max), the same shape the controller would resolve a single
// IOLimiter's VolumeLimit into.
func DeviceLimits(readBPS, writeBPS, readIOPS, writeIOPS int64) storagev1alpha1.DeviceLimits {
	var dl storagev1alpha1.DeviceLimits
	if readBPS > 0 {
		dl.ReadBPS = new(readBPS)
	}
	if writeBPS > 0 {
		dl.WriteBPS = new(writeBPS)
	}
	if readIOPS > 0 {
		dl.ReadIOPS = new(readIOPS)
	}
	if writeIOPS > 0 {
		dl.WriteIOPS = new(writeIOPS)
	}
	return dl
}

// ExpectedIOMaxLine renders the exact io.max line the kernel should hold
// for dev given DeviceLimits, via the same internal/iomax formatting the
// agent itself uses (K1/K2: always four keys).
func ExpectedIOMaxLine(dev string, dl storagev1alpha1.DeviceLimits) string {
	rule := iomax.Rule{
		ReadBPS:   i64top(dl.ReadBPS),
		WriteBPS:  i64top(dl.WriteBPS),
		ReadIOPS:  i64top(dl.ReadIOPS),
		WriteIOPS: i64top(dl.WriteIOPS),
	}
	return rule.Format(dev)
}

func i64top(p *int64) *uint64 {
	if p == nil {
		return nil
	}
	return new(uint64(*p))
}

// CreatePodIOLimit creates pil and registers a cleanup that releases and
// deletes it (mirroring D9's handshake, performed by hand): delete, wait
// for the agent to reset every owned device and mark Released, remove the
// finalizer, wait for the object to be gone. Failed tests keep the object
// (t.Failed() at cleanup time) for debugging. Agent-package-only: the
// operator package never hand-crafts a PodIOLimit.
func CreatePodIOLimit(t *testing.T, ctx context.Context, pil *storagev1alpha1.PodIOLimit) *storagev1alpha1.PodIOLimit {
	t.Helper()
	// C7/D27: the VAP only allows the controller SA to write the main
	// resource; this suite plays the controller by hand, so it must too.
	require.NoError(t, ControllerClient.Create(ctx, pil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("test failed: keeping PodIOLimit %s/%s for debugging", pil.Namespace, pil.Name)
			return
		}
		DeletePodIOLimit(t, context.Background(), pil.Namespace, pil.Name)
	})
	return pil
}

// WaitForPILReady polls a PodIOLimit until its Ready condition's reason is
// one of want, failing loudly (with diagnostics) on an unexpected
// terminal Failed.
func WaitForPILReady(
	t *testing.T, ctx context.Context, ns, name string, timeout time.Duration, want ...string,
) *storagev1alpha1.PodIOLimit {
	t.Helper()
	isWanted := func(r string) bool { return slices.Contains(want, r) }
	var pil storagev1alpha1.PodIOLimit
	PollUntil(t, timeout, fmt.Sprintf("podiolimit %s/%s ready", ns, name), func() (bool, string) {
		if err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pil); err != nil {
			return false, fmt.Sprintf("get error: %v", err)
		}
		reason := ReadyReasonOf(&pil)
		status := fmt.Sprintf("reason=%s cgroupPath=%s", reason, pil.Status.CgroupPath)
		if isWanted(reason) {
			return true, status
		}
		if reason == "Failed" && !isWanted("Failed") {
			DumpDiagnostics(t, ns, pil.Status.CgroupPath)
			t.Fatalf("podiolimit %s/%s reached unexpected reason %s, wanted one of %v", ns, name, reason, want)
		}
		return false, status
	})
	return &pil
}

// FindPodIOLimit returns the PodIOLimit owned by pod (D18 name), or nil if
// none exists yet.
func FindPodIOLimit(ctx context.Context, ns string, podName string, podUID types.UID) (*storagev1alpha1.PodIOLimit, error) {
	var pil storagev1alpha1.PodIOLimit
	err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: PodIOLimitName(podName, podUID)}, &pil)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pil, nil
}

// WaitForPodIOLimitGone polls until the pod's PodIOLimit no longer exists
// (the controller-driven D9 handshake completed: agent released, finalizer
// removed, object deleted).
func WaitForPodIOLimitGone(t *testing.T, ctx context.Context, ns, podName string, podUID types.UID, timeout time.Duration) {
	t.Helper()
	name := PodIOLimitName(podName, podUID)
	PollUntil(t, timeout, fmt.Sprintf("podiolimit %s/%s gone", ns, name), func() (bool, string) {
		var pil storagev1alpha1.PodIOLimit
		err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pil)
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		if err != nil {
			return false, err.Error()
		}
		deleting := pil.DeletionTimestamp != nil
		return false, fmt.Sprintf("deleting=%v devices=%d reason=%s", deleting, len(pil.Status.Devices), ReadyReasonOf(&pil))
	})
}

func ReadyReasonOf(pil *storagev1alpha1.PodIOLimit) string {
	for _, c := range pil.Status.Conditions {
		if c.Type == "Ready" {
			return c.Reason
		}
	}
	return "<none>"
}

// DeletePodIOLimit performs the D9 handshake by hand: delete, wait for the
// agent's release (status.devices empty, Ready=False/Released), remove the
// finalizer, wait for the object to be gone. Agent-package-only.
func DeletePodIOLimit(t *testing.T, ctx context.Context, ns, name string) {
	t.Helper()
	var pil storagev1alpha1.PodIOLimit
	if err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pil); err != nil {
		if apierrors.IsNotFound(err) {
			return
		}
		t.Logf("cleanup: get podiolimit %s/%s: %v", ns, name, err)
		return
	}
	if pil.DeletionTimestamp == nil {
		// C7/D27: only the controller SA may delete the main resource.
		if err := ControllerClient.Delete(ctx, &pil); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("cleanup: delete podiolimit %s/%s: %v", ns, name, err)
			return
		}
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pil); err != nil {
			if apierrors.IsNotFound(err) {
				return
			}
			t.Logf("cleanup: get podiolimit %s/%s: %v", ns, name, err)
			return
		}
		if len(pil.Status.Devices) == 0 && ReadyReasonOf(&pil) == "Released" {
			break
		}
		if time.Now().After(deadline) {
			t.Logf("cleanup: podiolimit %s/%s never released (devices=%d, reason=%s); removing finalizer anyway",
				ns, name, len(pil.Status.Devices), ReadyReasonOf(&pil))
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	patch := client.MergeFromWithOptions(pil.DeepCopy(), client.MergeFromWithOptimisticLock{})
	pil.Finalizers = removeString(pil.Finalizers, FinalizerName)
	// C7/D27: finalizer removal is a metadata update on the main resource.
	if err := ControllerClient.Patch(ctx, &pil, patch); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("cleanup: removing finalizer on podiolimit %s/%s: %v", ns, name, err)
	}
}

func removeString(ss []string, s string) []string {
	out := make([]string, 0, len(ss))
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}

// DDResult is one dd invocation's own reported summary.
type DDResult struct {
	Bytes   int64
	Elapsed time.Duration
}

var ddSummaryRE = regexp.MustCompile(`(\d+) bytes .*copied, ([0-9.]+) seconds`)

// DDThroughput runs `dd ... oflag=direct` (or iflag=direct for a read) of
// sizeMB inside pod/container, at mountPath/testfile, and parses dd's own
// summary line (busybox 1.37 dd: "N bytes (X) copied, S.SSSSSS seconds,
// R/s") rather than timing the kubectl exec round trip, so kubectl/exec
// overhead never pollutes the measurement.
func DDThroughput(t *testing.T, ns, pod, mountPath string, sizeMB int, direction string) DDResult {
	t.Helper()
	file := mountPath + "/ddtest"
	var script string
	switch direction {
	case "write":
		script = fmt.Sprintf("dd if=/dev/zero of=%s bs=1M count=%d oflag=direct conv=fsync 2>&1", file, sizeMB)
	case "read":
		script = fmt.Sprintf("dd if=%s of=/dev/null bs=1M count=%d iflag=direct 2>&1", file, sizeMB)
	default:
		t.Fatalf("DDThroughput: unknown direction %q", direction)
	}
	out, err := ExecPod(ns, pod, appLabelKey, script)
	require.NoErrorf(t, err, "dd in %s/%s: %s", ns, pod, out)

	m := ddSummaryRE.FindStringSubmatch(out)
	require.NotNilf(t, m, "dd output did not match the expected summary format: %q", out)
	bytesN, err := strconv.ParseInt(m[1], 10, 64)
	require.NoError(t, err)
	secs, err := strconv.ParseFloat(m[2], 64)
	require.NoError(t, err)
	return DDResult{Bytes: bytesN, Elapsed: time.Duration(secs * float64(time.Second))}
}

// DiskDevice returns the whole-disk "MAJ:MIN" of one of the two e2e loop
// devices, read from the node rather than assumed, since loop device
// numbers aren't guaranteed stable across cluster recreations.
func DiskDevice(t *testing.T, disk string) string {
	t.Helper()
	mnt := diskMount(disk)
	src, err := exec.Command("docker", "exec", WorkerNode, "findmnt", "-no", "SOURCE", mnt).CombinedOutput() //nolint:gosec // fixed binary, test-controlled args.
	require.NoErrorf(t, err, "findmnt %s: %s", mnt, src)
	loopDev := strings.TrimSpace(string(src))
	majMin, err := exec.Command("docker", "exec", WorkerNode, "lsblk", "-ndo", "MAJ:MIN", loopDev).CombinedOutput() //nolint:gosec // fixed binary, test-controlled args.
	require.NoErrorf(t, err, "lsblk %s: %s", loopDev, majMin)
	return strings.TrimSpace(string(majMin))
}

// WriteNodeIOMax writes a raw io.max line directly on the node, simulating
// a rule some other tool wrote in the same pod cgroup (used by
// TestAgentReleaseResetsOnlyOwned to prove the agent only resets what it
// owns, D8).
func WriteNodeIOMax(t *testing.T, cgroupPath, line string) {
	t.Helper()
	path := "/sys/fs/cgroup/" + strings.TrimPrefix(cgroupPath, "/") + "/io.max"
	out, err := exec.Command("docker", "exec", WorkerNode, "sh", "-c", //nolint:gosec // fixed binary, test-controlled args.
		"echo '"+line+"' > "+path).CombinedOutput()
	require.NoErrorf(t, err, "writing %q to %s: %s", line, path, out)
}

// ListPodEvents lists Events involving the given Pod in ns.
func ListPodEvents(ctx context.Context, ns, podName string) ([]corev1.Event, error) {
	sel := fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", podName)
	events, err := Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	return events.Items, nil
}

// ListIOLimiterEvents lists Events involving the given IOLimiter in ns.
func ListIOLimiterEvents(ctx context.Context, ns, name string) ([]corev1.Event, error) {
	sel := fmt.Sprintf("involvedObject.kind=IOLimiter,involvedObject.name=%s", name)
	events, err := Clientset.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	return events.Items, nil
}

// IOLimiterReadyReasonOf returns the Ready condition's reason, or
// "<none>" if unset.
func IOLimiterReadyReasonOf(l *storagev1alpha1.IOLimiter) string {
	for _, c := range l.Status.Conditions {
		if c.Type == "Ready" {
			return c.Reason
		}
	}
	return "<none>"
}

// WaitForIOLimiterReady polls an IOLimiter until its Ready condition's
// reason is one of want.
func WaitForIOLimiterReady(t *testing.T, ctx context.Context, ns, name string, timeout time.Duration, want ...string) *storagev1alpha1.IOLimiter {
	t.Helper()
	isWanted := func(r string) bool { return slices.Contains(want, r) }
	var l storagev1alpha1.IOLimiter
	PollUntil(t, timeout, fmt.Sprintf("iolimiter %s/%s ready", ns, name), func() (bool, string) {
		if err := K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &l); err != nil {
			return false, fmt.Sprintf("get error: %v", err)
		}
		reason := IOLimiterReadyReasonOf(&l)
		status := fmt.Sprintf("reason=%s matched=%d applied=%d failed=%d", reason, l.Status.MatchedPods, l.Status.AppliedPods, l.Status.FailedPods)
		return isWanted(reason), status
	})
	return &l
}

// SortedKeys returns the sorted keys of an io.max device->line map, for
// deterministic assertion messages.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ContainsLine reports whether content has line as one of its (trimmed)
// lines.
func ContainsLine(content, line string) bool {
	for l := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}
