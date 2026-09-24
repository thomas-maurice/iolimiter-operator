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
	"maps"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// F9: every watch source below fanned out to a full reconcile on any
// change at all (including metadata-only noise like managedFields or a
// resourceVersion-only churn) -- these predicates narrow each source down
// to the fields the reconcilers that consume it actually look at. Create/
// Delete always pass (a reconciler must see an object appear or disappear
// regardless of which fields changed); only Update is filtered.

// podRelevantChangePredicate passes a Pod Update only when a field either
// reconciler actually reads changed: labels (selector matching), phase
// (podTerminal), nodeName (scheduling/podLive), deletionTimestamp
// (podLive/F17), or the pod volume list (Compute's PVC/CSI resolution --
// pod.spec.volumes is itself immutable post-create, but ephemeral
// container/volume status quirks aside this also covers the length
// changing, the only observable way volumes could ever differ). Anything
// else (status.conditions churn, resourceVersion-only bumps, ...) is
// noise neither reconciler consults.
func podRelevantChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, ok1 := e.ObjectOld.(*corev1.Pod)
			newPod, ok2 := e.ObjectNew.(*corev1.Pod)
			if !ok1 || !ok2 {
				return true
			}
			return !maps.Equal(oldPod.Labels, newPod.Labels) ||
				oldPod.Status.Phase != newPod.Status.Phase ||
				oldPod.Spec.NodeName != newPod.Spec.NodeName ||
				(oldPod.DeletionTimestamp == nil) != (newPod.DeletionTimestamp == nil) ||
				len(oldPod.Spec.Volumes) != len(newPod.Spec.Volumes)
		},
	}
}

// pvcRelevantChangePredicate passes a PersistentVolumeClaim Update only on
// a phase or volumeName change (resolvePVCClaim/D19's only two inputs).
func pvcRelevantChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPVC, ok1 := e.ObjectOld.(*corev1.PersistentVolumeClaim)
			newPVC, ok2 := e.ObjectNew.(*corev1.PersistentVolumeClaim)
			if !ok1 || !ok2 {
				return true
			}
			return oldPVC.Status.Phase != newPVC.Status.Phase ||
				oldPVC.Spec.VolumeName != newPVC.Spec.VolumeName
		},
	}
}

// pvRelevantChangePredicate passes a PersistentVolume Update only when its
// source (the F4 hostPath check) or its claim binding changed -- a PV's
// source is normally immutable post-create, so in practice this mostly
// passes the Bound transition (ClaimRef appearing), which is exactly the
// moment resolvePVCClaim's earlier "not cached yet, fail open" read needs
// to be revisited.
func pvRelevantChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPV, ok1 := e.ObjectOld.(*corev1.PersistentVolume)
			newPV, ok2 := e.ObjectNew.(*corev1.PersistentVolume)
			if !ok1 || !ok2 {
				return true
			}
			oldClaim, newClaim := "", ""
			if oldPV.Spec.ClaimRef != nil {
				oldClaim = oldPV.Spec.ClaimRef.Namespace + "/" + oldPV.Spec.ClaimRef.Name
			}
			if newPV.Spec.ClaimRef != nil {
				newClaim = newPV.Spec.ClaimRef.Namespace + "/" + newPV.Spec.ClaimRef.Name
			}
			return oldClaim != newClaim ||
				!reflect.DeepEqual(oldPV.Spec.PersistentVolumeSource, newPV.Spec.PersistentVolumeSource)
		},
	}
}

// podIOLimitRelevantChangePredicate passes a PodIOLimit Update only on a
// spec generation change (PodReconciler's own patches), a status change
// (the agent's writes, which both reconcilers react to), or
// deletionTimestamp appearing (D9's handshake). A pure resourceVersion/
// managedFields churn with none of those is not worth a reconcile.
func podIOLimitRelevantChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPIL, ok1 := e.ObjectOld.(*storagev1alpha1.PodIOLimit)
			newPIL, ok2 := e.ObjectNew.(*storagev1alpha1.PodIOLimit)
			if !ok1 || !ok2 {
				return true
			}
			return oldPIL.Generation != newPIL.Generation ||
				(oldPIL.DeletionTimestamp == nil) != (newPIL.DeletionTimestamp == nil) ||
				!reflect.DeepEqual(oldPIL.Status, newPIL.Status)
		},
	}
}
