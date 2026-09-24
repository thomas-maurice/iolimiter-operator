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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/test/e2e/harness"
)

// TestVAP_MetadataOnlyUpdateAllowed proves the D33 carve-out (Fable review
// F3, "Uninstall" README section): a metadata-only UPDATE
// (object.spec == oldObject.spec) on podiolimits is allowed from any
// RBAC-permitted identity, not only the controller service account -- this
// is what lets a cluster-admin strip a stuck finalizer during uninstall
// without a Helm pre-delete hook. The identity here is the ambient kind
// context (harness.K8sClient), never impersonated as the controller SA, so
// a pass here can only be explained by the carve-out, not by "it was the
// controller all along".
func TestVAP_MetadataOnlyUpdateAllowed(t *testing.T) {
	ctx := context.Background()
	// The PodIOLimit below points at a non-existent pod: with the real
	// controller running, D30's orphan GC can delete it before the
	// metadata-only patch under test ever runs (see the package doc
	// comment).
	withControllerOff(t, ctx)
	ns := harness.NewTestNamespace(t, "vap-metadata-only")
	controllerUser := controllerSAUsername(t, ctx)

	pil := newTestPodIOLimit(ns, "metadata-only", harness.WorkerNode)
	// C7/D27: CREATE on the main resource is still controller-SA-only.
	require.NoError(t, harness.ControllerClient.Create(ctx, pil))
	t.Cleanup(func() {
		_, _ = harness.KubectlOutput("delete", "podiolimit", "-n", ns, pil.Name, "--as="+controllerUser, "--ignore-not-found")
	})

	var got storagev1alpha1.PodIOLimit
	require.NoError(t, harness.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: pil.Name}, &got))
	patch := client.MergeFrom(got.DeepCopy())
	if got.Labels == nil {
		got.Labels = map[string]string{}
	}
	got.Labels["hardening-e2e"] = "metadata-only"
	require.NoError(t, harness.K8sClient.Patch(ctx, &got, patch),
		"a metadata-only update (no spec change) must be allowed from any RBAC-permitted identity, not just the controller SA (SPEC.md D33)")
}

// TestVAP_SpecChangeStillDeniedForNonController proves the D33 carve-out is
// metadata-only, not "any UPDATE": the same ambient, non-controller
// identity that TestVAP_MetadataOnlyUpdateAllowed just proved can patch
// labels is still denied when the patch also changes spec.
func TestVAP_SpecChangeStillDeniedForNonController(t *testing.T) {
	ctx := context.Background()
	// The PodIOLimit below points at a non-existent pod: with the real
	// controller running, D30's orphan GC can delete it before the
	// spec-changing patch under test ever runs (see the package doc
	// comment).
	withControllerOff(t, ctx)
	ns := harness.NewTestNamespace(t, "vap-spec-change-denied")
	controllerUser := controllerSAUsername(t, ctx)

	pil := newTestPodIOLimit(ns, "spec-change-denied", harness.WorkerNode)
	require.NoError(t, harness.ControllerClient.Create(ctx, pil))
	t.Cleanup(func() {
		_, _ = harness.KubectlOutput("delete", "podiolimit", "-n", ns, pil.Name, "--as="+controllerUser, "--ignore-not-found")
	})

	var got storagev1alpha1.PodIOLimit
	require.NoError(t, harness.K8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: pil.Name}, &got))
	require.NotEmpty(t, got.Spec.Volumes, "test fixture must carry at least one volume to mutate")
	// A merge patch without resourceVersion: the controller is concurrently
	// reconciling this (orphan) object, so an Update carrying a stale RV can
	// lose with a 409 before admission ever evaluates it.
	patch := client.MergeFrom(got.DeepCopy())
	got.Spec.Volumes[0].KubeletDirName = "changed-by-e2e"

	err := harness.K8sClient.Patch(ctx, &got, patch)
	require.Error(t, err, "a spec-changing update from a non-controller identity must still be denied (SPEC.md D27, D33)")
	assert.Contains(t, err.Error(), "ValidatingAdmissionPolicy")
	assert.Contains(t, err.Error(), "podiolimit-write-restricted")
}
