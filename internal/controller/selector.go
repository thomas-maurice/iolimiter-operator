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
	"fmt"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/internal/controller/desired"
)

// D37 selector bounds: a podSelector this large or larger is rejected as
// InvalidSelector rather than parsed, the same bound the (rejected, over
// apiserver CEL cost budget by >100x -- see PodSelector's own doc comment)
// CEL rule would have enforced. 63 matches Kubernetes' own label key/value
// length limit, so this never rejects anything a real label selector could
// match anyway.
const (
	maxSelectorMatchExpressions = 16
	maxSelectorValues           = 64
	maxSelectorMatchLabels      = 16
	maxSelectorKeyValueLength   = 63
)

// selectorCacheEntry is one IOLimiter's cached, already-bounds-checked
// selector, valid as long as Generation matches (a spec edit bumps
// Generation, invalidating it).
type selectorCacheEntry struct {
	generation int64
	selector   labels.Selector
	err        error
}

// selectorCache parses each IOLimiter's podSelector at most once per
// generation (D37), instead of once per pod event: listMatchingLimiters
// runs once per pod reconcile, mapIOLimiterToPods once per pod in the
// namespace per IOLimiter event, and without this cache both re-parse and
// re-validate every limiter's selector from scratch every single time.
// Keyed by UID (stable across a limiter's lifetime, unlike name+namespace
// which a delete+recreate could reuse).
type selectorCache struct {
	mu      sync.Mutex
	entries map[types.UID]selectorCacheEntry
}

func newSelectorCache() *selectorCache {
	return &selectorCache{entries: make(map[types.UID]selectorCacheEntry)}
}

// globalSelectorCache is shared by every reconciler and mapping function in
// this package: there is exactly one podSelector per IOLimiter object
// regardless of who's asking.
//
// This intentionally never evicts a deleted limiter's entry (bounded, slow
// leak: one small entry per IOLimiter UID ever created, until the process
// restarts). D37 already asks operators to bound total IOLimiter count via
// a ResourceQuota for the DoS concern this cache itself exists to address;
// evicting on delete would need a reliable delete hook this package doesn't
// currently have (Reconcile's NotFound path only has the request's
// name/namespace, not the deleted object's UID). Noted as a known,
// deliberately accepted tradeoff rather than adding one.
var globalSelectorCache = newSelectorCache()

func (c *selectorCache) selectorFor(l *storagev1alpha1.IOLimiter) (labels.Selector, error) {
	c.mu.Lock()
	if e, ok := c.entries[l.UID]; ok && e.generation == l.Generation {
		c.mu.Unlock()
		return e.selector, e.err
	}
	c.mu.Unlock()

	sel, err := parseBoundedSelector(&l.Spec.PodSelector)

	c.mu.Lock()
	c.entries[l.UID] = selectorCacheEntry{generation: l.Generation, selector: sel, err: err}
	c.mu.Unlock()
	return sel, err
}

// parseBoundedSelector implements D37: the bound check runs BEFORE
// metav1.LabelSelectorAsSelector and before any per-pod matching, so a
// hostile selector is rejected in bounded string/list-length work, never
// reaching selector construction.
func parseBoundedSelector(ls *metav1.LabelSelector) (labels.Selector, error) {
	if err := validateSelectorBounds(ls); err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(ls)
}

func validateSelectorBounds(ls *metav1.LabelSelector) error {
	if len(ls.MatchExpressions) > maxSelectorMatchExpressions {
		return fmt.Errorf("matchExpressions has %d entries, more than %d", len(ls.MatchExpressions), maxSelectorMatchExpressions)
	}
	if len(ls.MatchLabels) > maxSelectorMatchLabels {
		return fmt.Errorf("matchLabels has %d entries, more than %d", len(ls.MatchLabels), maxSelectorMatchLabels)
	}
	for k, v := range ls.MatchLabels {
		if len(k) > maxSelectorKeyValueLength || len(v) > maxSelectorKeyValueLength {
			return fmt.Errorf("matchLabels key/value longer than %d characters", maxSelectorKeyValueLength)
		}
	}
	for _, e := range ls.MatchExpressions {
		if len(e.Key) > maxSelectorKeyValueLength {
			return fmt.Errorf("matchExpressions key %q longer than %d characters", e.Key, maxSelectorKeyValueLength)
		}
		if len(e.Values) > maxSelectorValues {
			return fmt.Errorf("matchExpressions[%q].values has %d entries, more than %d", e.Key, len(e.Values), maxSelectorValues)
		}
		for _, v := range e.Values {
			if len(v) > maxSelectorKeyValueLength {
				return fmt.Errorf("matchExpressions[%q] value longer than %d characters", e.Key, maxSelectorKeyValueLength)
			}
		}
	}
	return nil
}

// listMatchingLimiters lists every IOLimiter in pod's namespace whose
// podSelector matches pod. Shared by PodReconciler (to build the spec) and
// IOLimiterReconciler (to build status), so desired.Compute is always fed
// the same limiter set for the same pod (D20: "spec and status can't
// disagree").
func listMatchingLimiters(ctx context.Context, c client.Client, pod *corev1.Pod) ([]storagev1alpha1.IOLimiter, error) {
	var list storagev1alpha1.IOLimiterList
	if err := c.List(ctx, &list, client.InNamespace(pod.Namespace)); err != nil {
		return nil, fmt.Errorf("listing IOLimiters in %s: %w", pod.Namespace, err)
	}

	var out []storagev1alpha1.IOLimiter
	for _, l := range list.Items {
		sel, err := globalSelectorCache.selectorFor(&l)
		if err != nil {
			// An invalid/too-large selector can't match anything; skip
			// rather than fail the whole reconcile for every pod in the
			// namespace (D37: reported separately, on the limiter's own
			// status, by IOLimiterReconciler).
			continue
		}
		if sel.Matches(labels.Set(pod.Labels)) {
			out = append(out, l)
		}
	}
	return out, nil
}

// limitersForPod returns listMatchingLimiters' result, augmented per D37's
// update: a limiter that no longer selector-matches pod ONLY because its
// own podSelector is now invalid (parse error or over the D37 bounds), but
// which still exists and is already a contributing source for this pod's
// existingVolumes, is added back as if it still matched. Its Spec.Volumes
// rows are otherwise untouched -- valid rows keep contributing normally,
// and an invalid row still gets D36's own freeze via desired.Compute -- so
// an admin breaking a selector can never silently loosen an already-applied
// limit (the InvalidSelector analogue of D35/D36's InvalidLimit freeze).
//
// A limiter that's been deleted, or whose selector is valid but simply no
// longer matches pod, is not added back: those are genuine drops, and
// mapIOLimiterToPods already re-enqueues this pod (via the PodIOLimit's own
// "sources" index) when either happens, so the drop itself is picked up.
func limitersForPod(ctx context.Context, c client.Client, pod *corev1.Pod, existingVolumes []storagev1alpha1.PodVolumeLimit) ([]storagev1alpha1.IOLimiter, error) {
	matched, err := listMatchingLimiters(ctx, c, pod)
	if err != nil {
		return nil, err
	}
	if len(existingVolumes) == 0 {
		return matched, nil
	}

	matchedNames := make(map[string]struct{}, len(matched))
	for _, l := range matched {
		matchedNames[l.Name] = struct{}{}
	}

	sourceNames := make(map[string]struct{})
	for _, v := range existingVolumes {
		for _, contrib := range v.Contributions {
			sourceNames[contrib.Source] = struct{}{}
		}
	}

	for name := range sourceNames {
		if _, ok := matchedNames[name]; ok {
			continue
		}
		var l storagev1alpha1.IOLimiter
		if err := c.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: name}, &l); err != nil {
			if apierrors.IsNotFound(err) {
				continue // deleted: genuinely dropped
			}
			return nil, fmt.Errorf("getting IOLimiter %s/%s: %w", pod.Namespace, name, err)
		}
		if _, selErr := globalSelectorCache.selectorFor(&l); selErr == nil {
			continue // valid selector, just no longer matches: genuinely dropped
		}
		matched = append(matched, l)
	}
	return matched, nil
}

// pvcLookupFor builds a desired.PVCLookup backed by a live client Get, for
// the given namespace. Compute stays pure; this is the one place that
// bridges it to the cluster.
func pvcLookupFor(ctx context.Context, c client.Client, namespace string) desired.PVCLookup {
	return func(claimName string) (*corev1.PersistentVolumeClaim, bool) {
		var pvc corev1.PersistentVolumeClaim
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: claimName}, &pvc); err != nil {
			return nil, false
		}
		return &pvc, true
	}
}

// pvLookupFor builds a desired.PVLookup backed by a live client Get
// (PersistentVolumes are cluster-scoped, D16 cache). Compute stays pure;
// this is the one place that bridges it to the cluster (F4).
func pvLookupFor(ctx context.Context, c client.Client) desired.PVLookup {
	return func(pvName string) (*corev1.PersistentVolume, bool) {
		var pv corev1.PersistentVolume
		if err := c.Get(ctx, client.ObjectKey{Name: pvName}, &pv); err != nil {
			return nil, false
		}
		return &pv, true
	}
}

// podTerminal reports whether phase means the pod's containers (and its
// cgroup, per D9) are gone for good.
func podTerminal(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}
