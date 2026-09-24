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

package agent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/thomas-maurice/iolimiter-operator/test/e2e/harness"
)

// TestAgentDaemonSetIsNotPrivileged asserts D14 directly on the live,
// deployed DaemonSet spec: no privileged container, no hostPID/hostNetwork,
// capabilities dropped, no escalation, read-only rootfs. This is the
// acceptance criterion's "agent pod spec is asserted non-privileged in the
// test" -- every other test in this package additionally proves the
// consequence (the agent writes io.max successfully anyway, D14's actual
// risk).
func TestAgentDaemonSetIsNotPrivileged(t *testing.T) {
	ctx := context.Background()
	ds, err := harness.Clientset.AppsV1().DaemonSets(harness.AgentNamespace).Get(ctx, harness.AgentDaemonSet, metav1.GetOptions{})
	require.NoError(t, err)

	spec := ds.Spec.Template.Spec
	assert.False(t, spec.HostPID, "agent pod must not use hostPID (D14)")
	assert.False(t, spec.HostNetwork, "agent pod must not use hostNetwork (D14)")

	require.Len(t, spec.Containers, 1, "expected exactly one container in the agent pod")
	c := spec.Containers[0]
	require.NotNil(t, c.SecurityContext, "agent container must set a securityContext")
	sc := c.SecurityContext
	if assert.NotNil(t, sc.Privileged) {
		assert.False(t, *sc.Privileged, "agent container must not be privileged (D14)")
	}
	if assert.NotNil(t, sc.AllowPrivilegeEscalation) {
		assert.False(t, *sc.AllowPrivilegeEscalation, "agent container must not allow privilege escalation (D14)")
	}
	if assert.NotNil(t, sc.ReadOnlyRootFilesystem) {
		assert.True(t, *sc.ReadOnlyRootFilesystem, "agent container must have a read-only root filesystem (D14)")
	}
	require.NotNil(t, sc.Capabilities, "agent container must set capabilities.drop")
	assert.ElementsMatch(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop, "agent container must drop ALL capabilities (D14)")
	assert.Empty(t, sc.Capabilities.Add, "agent container must not add any capability back (D14)")
}
