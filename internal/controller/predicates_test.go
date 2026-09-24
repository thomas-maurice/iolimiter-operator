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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/stretchr/testify/assert"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// TestPodRelevantChangePredicate_F9 proves F9's exact rule set: a Pod
// Update only passes when labels, phase, nodeName, deletionTimestamp or
// the volume count changed -- anything else (here: a status condition
// churn with nothing else moving) must be filtered out, since neither
// reconciler reads it.
func TestPodRelevantChangePredicate_F9(t *testing.T) {
	base := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "apps", Labels: map[string]string{"app": "postgres"}},
			Spec:       corev1.PodSpec{NodeName: "node-a", Volumes: []corev1.Volume{{Name: "data"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	pred := podRelevantChangePredicate()

	t.Run("irrelevant status churn is filtered", func(t *testing.T) {
		oldPod, newPod := base(), base()
		newPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
	})
	t.Run("label change passes", func(t *testing.T) {
		oldPod, newPod := base(), base()
		newPod.Labels = map[string]string{"app": "redis"}
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
	})
	t.Run("phase change passes", func(t *testing.T) {
		oldPod, newPod := base(), base()
		newPod.Status.Phase = corev1.PodSucceeded
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
	})
	t.Run("nodeName change passes", func(t *testing.T) {
		oldPod, newPod := base(), base()
		newPod.Spec.NodeName = "node-b"
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
	})
	t.Run("deletionTimestamp appearing passes", func(t *testing.T) {
		oldPod, newPod := base(), base()
		now := metav1.NewTime(time.Now())
		newPod.DeletionTimestamp = &now
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
	})
	t.Run("volume count change passes", func(t *testing.T) {
		oldPod, newPod := base(), base()
		newPod.Spec.Volumes = append(newPod.Spec.Volumes, corev1.Volume{Name: "wal"})
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPod, ObjectNew: newPod}))
	})
	t.Run("create and delete always pass", func(t *testing.T) {
		assert.True(t, pred.Create(event.CreateEvent{Object: base()}))
		assert.True(t, pred.Delete(event.DeleteEvent{Object: base()}))
	})
}

// TestPVCRelevantChangePredicate_F9 proves the PVC predicate only reacts to
// its two documented inputs (D19: phase, volumeName).
func TestPVCRelevantChangePredicate_F9(t *testing.T) {
	base := func() *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "data-web-0", Namespace: "apps"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		}
	}
	pred := pvcRelevantChangePredicate()

	t.Run("irrelevant annotation churn is filtered", func(t *testing.T) {
		oldPVC, newPVC := base(), base()
		newPVC.Annotations = map[string]string{"foo": "bar"}
		assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: oldPVC, ObjectNew: newPVC}))
	})
	t.Run("phase change passes", func(t *testing.T) {
		oldPVC, newPVC := base(), base()
		newPVC.Status.Phase = corev1.ClaimBound
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPVC, ObjectNew: newPVC}))
	})
	t.Run("volumeName change passes", func(t *testing.T) {
		oldPVC, newPVC := base(), base()
		newPVC.Spec.VolumeName = "pv-1"
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPVC, ObjectNew: newPVC}))
	})
}

// TestPVRelevantChangePredicate_F9 proves the PV predicate reacts to a
// claim binding and to its source (F4's hostPath check), not to unrelated
// metadata.
func TestPVRelevantChangePredicate_F9(t *testing.T) {
	base := func() *corev1.PersistentVolume {
		return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-1"}}
	}
	pred := pvRelevantChangePredicate()

	t.Run("irrelevant annotation churn is filtered", func(t *testing.T) {
		oldPV, newPV := base(), base()
		newPV.Annotations = map[string]string{"foo": "bar"}
		assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: oldPV, ObjectNew: newPV}))
	})
	t.Run("claimRef appearing (Bound transition) passes", func(t *testing.T) {
		oldPV, newPV := base(), base()
		newPV.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "apps", Name: "data-web-0"}
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPV, ObjectNew: newPV}))
	})
	t.Run("source change passes", func(t *testing.T) {
		oldPV, newPV := base(), base()
		newPV.Spec.PersistentVolumeSource = corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}}
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPV, ObjectNew: newPV}))
	})
}

// TestPodIOLimitRelevantChangePredicate_F9 proves the PodIOLimit predicate
// reacts to a spec generation change, a status change, or deletionTimestamp
// appearing -- not to unrelated metadata churn.
func TestPodIOLimitRelevantChangePredicate_F9(t *testing.T) {
	base := func() *storagev1alpha1.PodIOLimit {
		return &storagev1alpha1.PodIOLimit{
			ObjectMeta: metav1.ObjectMeta{Name: "web-0-abcd1234", Namespace: "apps", Generation: 1},
		}
	}
	pred := podIOLimitRelevantChangePredicate()

	t.Run("irrelevant annotation churn is filtered", func(t *testing.T) {
		oldPIL, newPIL := base(), base()
		newPIL.Annotations = map[string]string{"foo": "bar"}
		assert.False(t, pred.Update(event.UpdateEvent{ObjectOld: oldPIL, ObjectNew: newPIL}))
	})
	t.Run("generation change passes", func(t *testing.T) {
		oldPIL, newPIL := base(), base()
		newPIL.Generation = 2
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPIL, ObjectNew: newPIL}))
	})
	t.Run("status change passes", func(t *testing.T) {
		oldPIL, newPIL := base(), base()
		newPIL.Status.Devices = []storagev1alpha1.OwnedDevice{{Device: "8:0", Rule: "rbps=max wbps=max riops=max wiops=max", State: "Applied"}}
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPIL, ObjectNew: newPIL}))
	})
	t.Run("deletionTimestamp appearing passes", func(t *testing.T) {
		oldPIL, newPIL := base(), base()
		oldPIL.Finalizers = []string{finalizerName}
		newPIL.Finalizers = []string{finalizerName}
		now := metav1.NewTime(time.Now())
		newPIL.DeletionTimestamp = &now
		assert.True(t, pred.Update(event.UpdateEvent{ObjectOld: oldPIL, ObjectNew: newPIL}))
	})
}
