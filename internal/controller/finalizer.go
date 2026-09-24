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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
)

// finalizerName is the deletion-handshake finalizer, owned by the
// controller (D9). The controller adds it at create time; only the
// controller ever removes it, either once the agent has released every
// device (normal path) or immediately when the pod it belongs to is gone
// or terminal (dead-node path).
const finalizerName = "storage.maurice.fr/io-max-reset"

// removeFinalizerOptimistic removes finalizerName from pil using
// client.MergeFromWithOptimisticLock (D17): a stale in-memory copy must
// conflict, not silently overwrite a concurrent write (e.g. the agent
// writing status the moment before). reason is purely for the log line
// (D9: "which D9 path" -- agent released vs pod gone/terminal).
//
// pil is re-Get before patching so the optimistic lock is keyed off a
// fresh resourceVersion: callers that just called Delete() on the very
// same object must not reuse the pre-Delete copy, since Delete (when a
// finalizer is present) only sets deletionTimestamp server-side and does
// not update the client's in-memory copy.
func (r *PodReconciler) removeFinalizerOptimistic(ctx context.Context, pil *storagev1alpha1.PodIOLimit, reason string) (bool, error) {
	log := logf.FromContext(ctx)

	var fresh storagev1alpha1.PodIOLimit
	if err := r.Get(ctx, client.ObjectKeyFromObject(pil), &fresh); err != nil {
		if apierrors.IsNotFound(err) {
			// Already gone (GC finished once the last finalizer was
			// removed by a previous, since-succeeded reconcile).
			return true, nil
		}
		return false, err
	}
	if !controllerutil.ContainsFinalizer(&fresh, finalizerName) {
		return true, nil
	}

	patch := client.MergeFromWithOptions(fresh.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.RemoveFinalizer(&fresh, finalizerName)
	if err := r.Patch(ctx, &fresh, patch); err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("Finalizer removal conflict, requeueing",
				"podiolimit", fresh.Namespace+"/"+fresh.Name, "finalizer", finalizerName)
			return false, nil
		}
		if apierrors.IsNotFound(err) {
			// Deleted by something else (e.g. ownerRef GC) between the Get
			// above and this Patch -- already gone is success, not a
			// reconcile error with backoff.
			log.V(1).Info("PodIOLimit already gone, nothing to remove finalizer from",
				"podiolimit", fresh.Namespace+"/"+fresh.Name, "finalizer", finalizerName, "reason", reason)
			return true, nil
		}
		return false, err
	}

	log.Info("Finalizer removed", "podiolimit", fresh.Namespace+"/"+fresh.Name,
		"finalizer", finalizerName, "reason", reason)
	return true, nil
}
