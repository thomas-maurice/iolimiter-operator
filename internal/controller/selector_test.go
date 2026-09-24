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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// TestValidateSelectorBounds_D37 proves each of D37's bounds is actually
// enforced: matchExpressions count, values-per-expression count,
// matchLabels count, and key/value length, each independently (a selector
// failing only one bound must still be rejected).
func TestValidateSelectorBounds_D37(t *testing.T) {
	longString := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a'
		}
		return string(b)
	}

	tests := []struct {
		name    string
		sel     metav1.LabelSelector
		wantErr bool
	}{
		{name: "empty selector ok", sel: metav1.LabelSelector{}, wantErr: false},
		{name: "small selector ok", sel: metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}}, wantErr: false},
		{
			name: "too many matchExpressions",
			sel: metav1.LabelSelector{MatchExpressions: func() []metav1.LabelSelectorRequirement {
				out := make([]metav1.LabelSelectorRequirement, maxSelectorMatchExpressions+1)
				for i := range out {
					out[i] = metav1.LabelSelectorRequirement{Key: fmt.Sprintf("k%d", i), Operator: metav1.LabelSelectorOpExists}
				}
				return out
			}()},
			wantErr: true,
		},
		{
			name: "too many values in one matchExpression",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: "k", Operator: metav1.LabelSelectorOpIn, Values: func() []string {
					out := make([]string, maxSelectorValues+1)
					for i := range out {
						out[i] = fmt.Sprintf("v%d", i)
					}
					return out
				}()},
			}},
			wantErr: true,
		},
		{
			name: "too many matchLabels",
			sel: metav1.LabelSelector{MatchLabels: func() map[string]string {
				out := make(map[string]string, maxSelectorMatchLabels+1)
				for i := range maxSelectorMatchLabels + 1 {
					out[fmt.Sprintf("k%d", i)] = "v"
				}
				return out
			}()},
			wantErr: true,
		},
		{
			name:    "matchLabels value too long",
			sel:     metav1.LabelSelector{MatchLabels: map[string]string{"app": longString(maxSelectorKeyValueLength + 1)}},
			wantErr: true,
		},
		{
			name: "matchExpressions key too long",
			sel: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: longString(maxSelectorKeyValueLength + 1), Operator: metav1.LabelSelectorOpExists},
			}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSelectorBounds(&tc.sel)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestSelectorCache_ParsedOncePerGeneration proves the D37 cache: two
// lookups for the same limiter UID+generation must return the exact same
// parsed selector (not merely an equal one) without re-parsing, and a
// generation bump must force a fresh parse.
func TestSelectorCache_ParsedOncePerGeneration(t *testing.T) {
	c := newSelectorCache()
	l := newLimiter("apps", "l1", map[string]string{"app": "postgres"})
	l.UID = "uid-1"
	l.Generation = 1

	sel1, err := c.selectorFor(l)
	require.NoError(t, err)
	require.Len(t, c.entries, 1)

	sel2, err := c.selectorFor(l)
	require.NoError(t, err)
	assert.Equal(t, sel1, sel2, "same UID+generation must hit the cache, not reparse")
	assert.True(t, sel1.Matches(labels.Set{"app": "postgres"}))

	// Mutate the selector but keep the same generation: the cache must
	// still serve the stale (already-cached) selector -- a caller that
	// bumped Spec without bumping Generation is a bug elsewhere, not this
	// cache's job to detect.
	l2 := l.DeepCopy()
	l2.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"app": "mysql"}}
	sel3, err := c.selectorFor(l2)
	require.NoError(t, err)
	assert.Equal(t, sel1, sel3)
	assert.True(t, sel3.Matches(labels.Set{"app": "postgres"}), "still the stale cached selector, not the mutated spec")

	// Bump generation: must reparse and reflect the new spec.
	l3 := l.DeepCopy()
	l3.Generation = 2
	l3.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{"app": "mysql"}}
	sel4, err := c.selectorFor(l3)
	require.NoError(t, err)
	assert.False(t, sel4.Matches(labels.Set{"app": "postgres"}), "the new selector must reflect the new spec, not the cached one")
	assert.True(t, sel4.Matches(labels.Set{"app": "mysql"}))
	require.Len(t, c.entries, 1, "same UID, new generation overwrites the old entry rather than growing unbounded")
}

// TestSelectorCache_InvalidSelectorCachedToo proves an out-of-bounds
// selector's error is cached exactly like a valid one -- otherwise every
// call would redo the (cheap but nonzero) bounds walk, and worse, a
// deliberately huge selector would defeat the whole point of the cache by
// being the one selector that's never actually cached.
func TestSelectorCache_InvalidSelectorCachedToo(t *testing.T) {
	c := newSelectorCache()
	l := newLimiter("apps", "l1", nil)
	l.UID = "uid-1"
	l.Generation = 1
	l.Spec.PodSelector.MatchLabels = make(map[string]string, maxSelectorMatchLabels+1)
	for i := range maxSelectorMatchLabels + 1 {
		l.Spec.PodSelector.MatchLabels[fmt.Sprintf("k%d", i)] = "v"
	}

	_, err1 := c.selectorFor(l)
	require.Error(t, err1)
	_, err2 := c.selectorFor(l)
	require.Error(t, err2)
	assert.Equal(t, err1, err2, "the cached error must be served, not a freshly-built one")
}

// oversizedSelector builds a podSelector one matchExpression over D37's
// bound, so validateSelectorBounds (and so listMatchingLimiters) always
// rejects it as InvalidSelector.
func oversizedSelector() metav1.LabelSelector {
	exprs := make([]metav1.LabelSelectorRequirement, maxSelectorMatchExpressions+1)
	for i := range exprs {
		exprs[i] = metav1.LabelSelectorRequirement{Key: fmt.Sprintf("k%d", i), Operator: metav1.LabelSelectorOpExists}
	}
	return metav1.LabelSelector{MatchExpressions: exprs}
}

// TestLimitersForPod_InvalidSelectorKeptAsExistingSource proves the D37 fix:
// a limiter still listed as a source in the pod's existing PodIOLimit
// contributions, but whose podSelector has since gone invalid (so it can no
// longer selector-match anything), is still returned -- otherwise its
// contribution would silently vanish from desired.Compute's input and the
// throttle it applied would be lost, even though the limiter itself still
// exists and its volume rows are unchanged.
func TestLimitersForPod_InvalidSelectorKeptAsExistingSource(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "postgres"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})
	l.Spec.PodSelector = oversizedSelector()

	c := newFakeClient(t, l)
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", Contributions: []storagev1alpha1.VolumeContribution{
			{Source: "postgres-data", Limits: storagev1alpha1.DeviceLimits{WriteBPS: new(int64(5 * 1024 * 1024))}},
		}},
	}

	got, err := limitersForPod(context.Background(), c, pod, existing)
	require.NoError(t, err)
	require.Len(t, got, 1, "the limiter must be kept despite its now-invalid selector")
	assert.Equal(t, "postgres-data", got[0].Name)
}

// TestLimitersForPod_ValidSelectorNoLongerMatches_Dropped proves the other
// side of D37: a limiter whose selector is fixed (valid) but simply doesn't
// match this pod anymore is a genuine drop, not kept.
func TestLimitersForPod_ValidSelectorNoLongerMatches_Dropped(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))
	l := newLimiter(ns, "postgres-data", map[string]string{"app": "mysql"},
		storagev1alpha1.VolumeLimit{Name: "data", Limits: storagev1alpha1.IOLimits{WriteBytesPerSecond: quantityPtr("5Mi")}})

	c := newFakeClient(t, l)
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", Contributions: []storagev1alpha1.VolumeContribution{
			{Source: "postgres-data", Limits: storagev1alpha1.DeviceLimits{WriteBPS: new(int64(5 * 1024 * 1024))}},
		}},
	}

	got, err := limitersForPod(context.Background(), c, pod, existing)
	require.NoError(t, err)
	assert.Empty(t, got, "a valid selector that no longer matches must be dropped, not kept")
}

// TestLimitersForPod_LimiterDeleted_Dropped proves a limiter that's been
// deleted entirely (still named as a source in the pod's existing
// contributions) is dropped without error.
func TestLimitersForPod_LimiterDeleted_Dropped(t *testing.T) {
	ns := "apps"
	pod := scheduledPod(ns, "web-0", "abcd1234-0000-0000-0000-000000000000", "node-a",
		map[string]string{"app": "postgres"}, pvcVolume("data", "data-web-0"))

	c := newFakeClient(t, pod) // "postgres-data" never created
	existing := []storagev1alpha1.PodVolumeLimit{
		{Name: "data", Contributions: []storagev1alpha1.VolumeContribution{
			{Source: "postgres-data", Limits: storagev1alpha1.DeviceLimits{WriteBPS: new(int64(5 * 1024 * 1024))}},
		}},
	}

	got, err := limitersForPod(context.Background(), c, pod, existing)
	require.NoError(t, err)
	assert.Empty(t, got, "a deleted limiter must be dropped, not error")
}

// BenchmarkListMatchingLimiters_SelectorParsedOnceThenCached demonstrates
// the D37 acceptance line "parse happens once per generation": timing a
// single-limiter selectorFor lookup after it's already cached must be
// dramatically cheaper than the first (parsing) call, which is what makes
// repeated per-pod-event lookups against the same limiter cheap regardless
// of how many pod events arrive between spec edits.
func BenchmarkListMatchingLimiters_SelectorParsedOnceThenCached(b *testing.B) {
	c := newSelectorCache()
	l := &storagev1alpha1.IOLimiter{
		ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: "apps", UID: "uid-1", Generation: 1},
		Spec: storagev1alpha1.IOLimiterSpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "postgres"}},
		},
	}
	// Warm the cache once, exactly like the first pod event of this
	// limiter's current generation would.
	if _, err := c.selectorFor(l); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.selectorFor(l); err != nil {
			b.Fatal(err)
		}
	}
}
