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

package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
)

// TestReadyReason covers the coordinator's C2 review decision: a PodIOLimit
// whose every volume is Unsupported must not report Ready=True/Applied
// (nothing was actually applied), but a volume that's Unsupported
// alongside at least one Applied volume doesn't block Ready -- the agent
// did everything it could for the volumes it could act on.
func TestReadyReason(t *testing.T) {
	cases := []struct {
		name       string
		volumes    []storagev1alpha1.VolumeStatus
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "all Applied",
			volumes:    []storagev1alpha1.VolumeStatus{{State: stateApplied}, {State: stateApplied}},
			wantStatus: metav1.ConditionTrue,
			wantReason: stateApplied,
		},
		{
			name:       "any Pending wins over Applied",
			volumes:    []storagev1alpha1.VolumeStatus{{State: stateApplied}, {State: statePending}},
			wantStatus: metav1.ConditionFalse,
			wantReason: statePending,
		},
		{
			name:       "any Failed wins over Pending and Applied",
			volumes:    []storagev1alpha1.VolumeStatus{{State: stateApplied}, {State: statePending}, {State: stateFailed}},
			wantStatus: metav1.ConditionFalse,
			wantReason: stateFailed,
		},
		{
			name:       "every volume Unsupported is not misleadingly Applied",
			volumes:    []storagev1alpha1.VolumeStatus{{State: stateUnsupported}, {State: stateUnsupported}},
			wantStatus: metav1.ConditionFalse,
			wantReason: stateUnsupported,
		},
		{
			name:       "Unsupported alongside Applied does not block Ready",
			volumes:    []storagev1alpha1.VolumeStatus{{State: stateApplied}, {State: stateUnsupported}},
			wantStatus: metav1.ConditionTrue,
			wantReason: stateApplied,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := readyReason(tc.volumes)
			assert.Equal(t, tc.wantStatus, status)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
}
