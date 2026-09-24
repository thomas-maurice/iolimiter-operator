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

package hardening

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// controllerSAUsername/agentSAUsername fetch the real, live ServiceAccounts
// (rather than hardcoding "system:serviceaccount:<ns>:<name>" a second
// time) so a renamed ServiceAccount fails this test loudly instead of
// silently testing an identity nobody uses (SPEC.md D27's own comment in
// config/vap promises this).
func controllerSAUsername(t *testing.T, ctx context.Context) string {
	t.Helper()
	sa, err := harness.Clientset.CoreV1().ServiceAccounts(harness.AgentNamespace).Get(ctx, harness.ControllerDeployment, metav1.GetOptions{})
	require.NoErrorf(t, err, "controller ServiceAccount %s/%s not found", harness.AgentNamespace, harness.ControllerDeployment)
	return fmt.Sprintf("system:serviceaccount:%s:%s", sa.Namespace, sa.Name)
}

func agentSAUsername(t *testing.T, ctx context.Context) string {
	t.Helper()
	sa, err := harness.Clientset.CoreV1().ServiceAccounts(harness.AgentNamespace).Get(ctx, harness.AgentDaemonSet, metav1.GetOptions{})
	require.NoErrorf(t, err, "agent ServiceAccount %s/%s not found", harness.AgentNamespace, harness.AgentDaemonSet)
	return fmt.Sprintf("system:serviceaccount:%s:%s", sa.Namespace, sa.Name)
}

// applyYAML writes obj to a temp file and kubectl-applies it with extraArgs
// (used for --as/--as-user-extra impersonation and --subresource=status),
// returning kubectl's combined output and error.
func applyYAML(t *testing.T, obj any, extraArgs ...string) (string, error) {
	t.Helper()
	b, err := yaml.Marshal(obj)
	require.NoError(t, err)
	f := filepath.Join(t.TempDir(), "obj.yaml")
	require.NoError(t, os.WriteFile(f, b, 0o600))
	args := append([]string{"apply", "-f", f}, extraArgs...)
	return harness.KubectlOutput(args...)
}

// hasReason mirrors test/e2e/operator's helper of the same name (not
// shared: each e2e package is a standalone `go test` binary per SPEC.md
// §11 C5's package split).
func hasReason(events []corev1.Event, reason string) bool {
	for _, e := range events {
		if e.Reason == reason {
			return true
		}
	}
	return false
}

func newTestPodIOLimit(ns, name, nodeName string) *storagev1alpha1.PodIOLimit {
	return &storagev1alpha1.PodIOLimit{
		TypeMeta: metav1.TypeMeta{APIVersion: "storage.maurice.fr/v1alpha1", Kind: "PodIOLimit"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: nodeName,
			PodName:  "does-not-exist",
			PodUID:   types.UID("11111111-1111-1111-1111-111111111111"),
			QOSClass: "BestEffort",
			Volumes: []storagev1alpha1.PodVolumeLimit{{
				Name:           "data",
				KubeletDirName: "data",
				Sources:        []string{"dummy"},
			}},
		},
	}
}

// TestVAP_UserWithRBACCreateStillDenied proves the VAP (SPEC.md D27) is the
// blocking layer, not an accidental absence of RBAC: it grants a test
// identity full create/update/delete RBAC on podiolimits in its own
// namespace (so a plain "Forbidden" from RBAC can never explain a denial
// here), then asserts kubectl --as that identity is still denied, with the
// apiserver naming the policy in its message (C7's Logging acceptance:
// "VAP denials are visible in the e2e output").
func TestVAP_UserWithRBACCreateStillDenied(t *testing.T) {
	ctx := context.Background()
	ns := harness.NewTestNamespace(t, "vap-user")

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "podiolimit-full-access", Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"storage.maurice.fr"},
			Resources: []string{"podiolimits"},
			Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
		}},
	}
	require.NoError(t, harness.K8sClient.Create(ctx, role))
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "podiolimit-full-access", Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "e2e-tenant", APIGroup: "rbac.authorization.k8s.io"}},
	}
	require.NoError(t, harness.K8sClient.Create(ctx, binding))

	pil := newTestPodIOLimit(ns, "user-denied", harness.WorkerNode)
	out, err := applyYAML(t, pil, "--as=e2e-tenant")
	require.Errorf(t, err, "expected kubectl apply --as=e2e-tenant to be denied, got: %s", out)
	assert.Contains(t, out, "ValidatingAdmissionPolicy", "denial should name the VAP (C7 Logging acceptance)")
	assert.Contains(t, out, "podiolimit-write-restricted")
	assert.Contains(t, out, "controller service account")
}

// TestVAP_AgentWrongNodeStatusDenied proves the podiolimits/status policy
// checks the agent token's own node-name claim against spec.nodeName
// (SPEC.md D27, OQ2), not just the agent SA identity: an agent-SA identity
// impersonating a *different* node than the object's spec.nodeName is
// denied even though the SA itself is the right one.
func TestVAP_AgentWrongNodeStatusDenied(t *testing.T) {
	ctx := context.Background()
	// The PodIOLimit below points at a non-existent pod: with the real
	// controller running, D30's orphan GC can delete it before the status
	// patch under test ever runs (see the package doc comment).
	withControllerOff(t, ctx)
	ns := harness.NewTestNamespace(t, "vap-agent")
	controllerUser := controllerSAUsername(t, ctx)
	agentUser := agentSAUsername(t, ctx)

	pil := newTestPodIOLimit(ns, "agent-wrong-node", harness.WorkerNode)
	out, err := applyYAML(t, pil, "--as="+controllerUser)
	require.NoErrorf(t, err, "creating the PodIOLimit as the controller SA (setup, not under test) failed: %s", out)
	t.Cleanup(func() {
		_, _ = harness.KubectlOutput("delete", "podiolimit", "-n", ns, pil.Name, "--as="+controllerUser, "--ignore-not-found")
	})

	statusPatch := fmt.Sprintf(`{"status":{"cgroupPath":"forged-by-%s"}}`, harness.KindCluster)
	out, err = harness.KubectlOutput(
		"-n", ns, "patch", "podiolimit", pil.Name,
		"--type=merge", "--subresource=status",
		"--as="+agentUser,
		"--as-user-extra=authentication.kubernetes.io/node-name="+harness.KindCluster+"-control-plane", // pil.Spec.NodeName is WorkerNode, not this.
		"-p", statusPatch,
	)
	require.Errorf(t, err, "expected the status patch (wrong node-name claim) to be denied, got: %s", out)
	assert.Contains(t, out, "ValidatingAdmissionPolicy")
	assert.Contains(t, out, "podiolimit-status-write-restricted")
	assert.Contains(t, out, "own spec.nodeName")

	// The same patch as the *real*, legitimate agent (no impersonation)
	// against its own node's objects keeps working throughout normal
	// operation -- exercised continuously by every other e2e package's
	// suite staying green (C7's "the legitimate agent and controller
	// still work" acceptance), not re-proven here to avoid duplicating
	// that coverage.
}

// TestVAP_AdminCanDeletePodIOLimit proves the D27 reversal (fix batch,
// 2026-09-23): DELETE on podiolimits is no longer VAP-restricted. An
// ambient cluster-admin identity (harness.K8sClient, no impersonation) can
// delete a real, controller-created PodIOLimit for a pod that still
// exists, and the D9 finalizer still makes the agent reset io.max first --
// deleting it is not a bypass of the reset, only of *when* it happens. This
// is also the fix for the reported bug: the same admission gap previously
// blocked Kubernetes' own garbage collector and namespace controller
// (SPEC.md D27's updated rationale), which this test can't reach directly
// from a unit-of-behavior e2e, but the identical code path (no
// username/operation restriction left on DELETE at all) covers both.
func TestVAP_AdminCanDeletePodIOLimit(t *testing.T) {
	ctx := context.Background()
	ns := harness.NewTestNamespace(t, "vap-admin-delete")

	dir := harness.MkTestDir(t, "disk0", "vap-admin-delete")
	pv := harness.NewLocalPV(ns+"-pv", dir)
	require.NoError(t, harness.K8sClient.Create(ctx, pv))
	t.Cleanup(func() { _ = harness.K8sClient.Delete(context.Background(), pv) })

	pvc := harness.NewLocalPVC(ns, "data", pv.Name)
	require.NoError(t, harness.K8sClient.Create(ctx, pvc))

	pod := harness.NewDataPod(ns, "app", []harness.VolMount{{Name: "data", PVC: pvc.Name}}, nil)
	require.NoError(t, harness.K8sClient.Create(ctx, pod))
	pod = harness.WaitPodRunning(t, ctx, ns, pod.Name, 3*time.Minute)

	limiter := harness.NewIOLimiter(ns, "limit", map[string]string{"app": "app"}, []storagev1alpha1.VolumeLimit{
		harness.VolumeLimit("data", 0, 4*1024*1024, 0, 0),
	})
	require.NoError(t, harness.K8sClient.Create(ctx, limiter))
	harness.WaitForIOLimiterReady(t, ctx, ns, limiter.Name, 2*time.Minute, "AllApplied")

	pil, err := harness.FindPodIOLimit(ctx, ns, pod.Name, pod.UID)
	require.NoError(t, err)
	require.NotNil(t, pil)
	harness.WaitForPILReady(t, ctx, ns, pil.Name, 2*time.Minute, "Applied")

	// The ambient identity here is the kind-admin kubeconfig context
	// (harness.K8sClient), i.e. a real cluster-admin -- exactly the
	// identity the reported bug found denied ("forbidden:
	// ValidatingAdmissionPolicy '...-podiolimit-write-restricted'" on a
	// routine `kubectl delete podiolimit`).
	require.NoError(t, harness.K8sClient.Delete(ctx, pil), "an ambient cluster-admin must be able to delete a PodIOLimit (D27 reversal)")

	// The finalizer must still make the agent reset io.max before the
	// object is actually gone -- deleting it doesn't bypass D9, it only
	// removes the extra admission-time restriction on *who* may ask for
	// it. IOLimitReset is the agent's own event for exactly this, D26.
	harness.PollUntil(t, 2*time.Minute, "pod gets an IOLimitReset event (agent released before the object disappeared)", func() (bool, string) {
		events, err := harness.ListPodEvents(ctx, ns, pod.Name)
		if err != nil {
			return false, err.Error()
		}
		return hasReason(events, "IOLimitReset"), "no IOLimitReset event yet"
	})
}

// TestOrphanPodIOLimitGarbageCollected is the e2e regression for the
// reported bug (SPEC.md D30): a PodIOLimit created directly as the
// controller SA (so it isn't itself blocked by the D27 CREATE-restricted
// VAP) with **no ownerRef** and a bogus pod UID/name -- exactly the shape
// of a leftover the controller never created and would previously never
// have noticed, since it was only ever reachable via Owns()' ownerRef
// mapping -- must still be released and deleted by the real, unmodified
// controller within a bounded time.
func TestOrphanPodIOLimitGarbageCollected(t *testing.T) {
	ctx := context.Background()
	ns := harness.NewTestNamespace(t, "orphan-gc")

	orphan := newTestPodIOLimit(ns, "orphan-no-ownerref", harness.WorkerNode)
	orphan.Finalizers = []string{harness.FinalizerName}
	// newTestPodIOLimit already sets a nonexistent podName/podUID; no
	// OwnerReferences are set, matching the reported leftover exactly.
	require.NoError(t, harness.ControllerClient.Create(ctx, orphan))

	harness.PollUntil(t, 30*time.Second, "orphan PodIOLimit is garbage-collected", func() (bool, string) {
		var got storagev1alpha1.PodIOLimit
		err := harness.K8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: orphan.Name}, &got)
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "still present"
	})
}

// TestVAPPolicies_MatchAllServedVersions proves the apiVersions fix (fix
// batch, 2026-09-23): both VAPs list "*" (or, if ever pinned again, every
// version storage.maurice.fr/PodIOLimit actually serves), not a stale
// literal list that silently stops protecting the resource the day a new
// version ships. Reads the live objects rather than the source YAML, so a
// hand-edit reverting to a pinned list -- or a new served version nobody
// updated this file for -- fails this test loudly.
func TestVAPPolicies_MatchAllServedVersions(t *testing.T) {
	ctx := context.Background()

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, harness.K8sClient.Get(ctx, types.NamespacedName{Name: "podiolimits.storage.maurice.fr"}, &crd))
	served := map[string]bool{}
	for _, v := range crd.Spec.Versions {
		if v.Served {
			served[v.Name] = true
		}
	}
	require.NotEmpty(t, served)

	for _, name := range []string{"iolimiter-operator-podiolimit-write-restricted", "iolimiter-operator-podiolimit-status-write-restricted"} {
		var vap admissionregistrationv1.ValidatingAdmissionPolicy
		require.NoError(t, harness.K8sClient.Get(ctx, types.NamespacedName{Name: name}, &vap))
		require.NotEmpty(t, vap.Spec.MatchConstraints.ResourceRules)
		apiVersions := vap.Spec.MatchConstraints.ResourceRules[0].APIVersions
		if slices.Contains(apiVersions, "*") {
			continue
		}
		for v := range served {
			assert.Containsf(t, apiVersions, v, "VAP %s must cover every served version of PodIOLimit; missing %s", name, v)
		}
	}
}
