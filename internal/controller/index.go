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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// Field index names (D16). Registered once by SetupIndexes and shared by
// both reconcilers. The extractor funcs are named (not inlined) so unit
// tests can register the same indexes on the fake client via
// fake.ClientBuilder.WithIndex, keeping the fake client's indexing behavior
// identical to the real manager's.
const (
	// indexPodIOLimitByPodName indexes PodIOLimit by spec.podName, so
	// PodReconciler can find every PodIOLimit for a given pod name
	// (including ones left over from a previous pod UID) without a
	// namespace-wide List+filter.
	indexPodIOLimitByPodName = "spec.podName"
	// indexPodIOLimitBySource indexes PodIOLimit by every IOLimiter name
	// listed in any of its volumes[].sources, so IOLimiterReconciler (and
	// PodReconciler, to re-evaluate pods a deleted/edited limiter used to
	// affect, D10) can find affected PodIOLimits without scanning every
	// object in the namespace.
	indexPodIOLimitBySource = "spec.volumes.sources"
	// indexPodByPVCClaimName indexes Pod by every PVC claim name it
	// references (persistentVolumeClaim and generic ephemeral), so a PVC
	// watch (e.g. Bound transition) can find the pods that care about it.
	indexPodByPVCClaimName = "spec.pvcClaimNames"
)

func indexPodIOLimitByPodNameFunc(obj client.Object) []string {
	pil := obj.(*storagev1alpha1.PodIOLimit)
	if pil.Spec.PodName == "" {
		return nil
	}
	return []string{pil.Spec.PodName}
}

func indexPodIOLimitBySourceFunc(obj client.Object) []string {
	pil := obj.(*storagev1alpha1.PodIOLimit)
	seen := map[string]struct{}{}
	var out []string
	for _, v := range pil.Spec.Volumes {
		for _, s := range v.Sources {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func indexPodByPVCClaimNameFunc(obj client.Object) []string {
	pod := obj.(*corev1.Pod)
	var out []string
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil:
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		case v.Ephemeral != nil:
			out = append(out, pod.Name+"-"+v.Name)
		}
	}
	return out
}

// SetupIndexes registers the field indexes both reconcilers rely on (D16).
// Call once per manager before starting either reconciler.
func SetupIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &storagev1alpha1.PodIOLimit{}, indexPodIOLimitByPodName, indexPodIOLimitByPodNameFunc); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &storagev1alpha1.PodIOLimit{}, indexPodIOLimitBySource, indexPodIOLimitBySourceFunc); err != nil {
		return err
	}
	return indexer.IndexField(ctx, &corev1.Pod{}, indexPodByPVCClaimName, indexPodByPVCClaimNameFunc)
}
