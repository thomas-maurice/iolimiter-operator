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
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/controller/desired"
)

// pilRequeueInterval paces the wait for a Terminating PodIOLimit still
// held by the agent (D9's handshake): a quick, bounded poll rather than
// relying solely on the watch (the object's own status update already
// triggers a requeue; this is a bound in case that races).
const pilRequeueInterval = 2 * time.Second

// requeueSoon retries a reconcile that hit an optimistic-lock conflict:
// the object's own change already triggers a watch-driven requeue, this is
// just a bound in case that races (ctrl.Result.Requeue is deprecated in
// favor of a short RequeueAfter).
const requeueSoon = 100 * time.Millisecond

// managedByLabel marks every PodIOLimit this controller owns.
const managedByLabel = "app.kubernetes.io/managed-by"

// managedByValue is managedByLabel's value.
const managedByValue = "k8s-blkio-limiter"

// PodReconciler reconciles a Pod object into at most one PodIOLimit,
// implementing SPEC.md §6.2's PodReconciler algorithm.
type PodReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits IOLimiter events (D26: PodIOLimitCreated/Deleted are
	// events on the IOLimiter(s) that caused the change, not on the Pod --
	// the agent owns Pod events).
	Recorder events.EventRecorder
	// APIReader is an uncached, direct client (mgr.GetAPIReader()). Used
	// only as a fallback when the cached Client reports a pod missing, to
	// tell "genuinely gone" apart from "not yet in my informer's cache"
	// (D30's race fix for orphan PodIOLimit cleanup). Optional: nil skips
	// the fallback (e.g. in unit tests using only a fake client), matching
	// D16's "no other type is ever read through the client" spirit -- this
	// is the one deliberate exception, gated on the cache having already
	// said "not found".
	APIReader client.Reader
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.maurice.fr,resources=podiolimits,verbs=get;list;watch;create;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile implements SPEC.md §6.2's PodReconciler: it computes the
// desired PodIOLimit for one pod and creates/patches/deletes it, and
// releases the D9 finalizer either through the agent's handshake (normal
// path) or directly when the pod is gone/terminal/recreated (dead path).
func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("pod", req.Namespace+"/"+req.Name)
	ctx = logf.IntoContext(ctx, log)

	var pod corev1.Pod
	podExists, err := r.getPod(ctx, req.NamespacedName, &pod)
	if err != nil {
		return ctrl.Result{}, err
	}

	var pilList storagev1alpha1.PodIOLimitList
	if err := r.List(ctx, &pilList, client.InNamespace(req.Namespace),
		client.MatchingFields{indexPodIOLimitByPodName: req.Name}); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing PodIOLimits for pod: %w", err)
	}

	// F17: podLive deliberately does NOT also require
	// pod.DeletionTimestamp == nil. A pod gets a DeletionTimestamp the
	// instant `kubectl delete` (or a StatefulSet rolling update) is
	// issued, but its cgroup -- and therefore any kernel rule still owned
	// via this pod's PodIOLimit -- survives for the whole grace period
	// while containers actually stop. If podLive folded
	// DeletionTimestamp in here, step 3 below would immediately take the
	// releaseDirect path (delete + strip the finalizer with no wait) the
	// very moment a graceful delete starts, exactly the same way it
	// already does for a genuinely dead pod -- but D9's whole point is
	// that direct release is only safe once there is no live cgroup left
	// to reset. Stripping the finalizer while the cgroup (and the kernel
	// rule) still exists would abandon that rule with no record of it,
	// which is precisely the invariant D8/D9 exist to prevent. So a
	// Terminating-but-not-yet-terminal pod must keep going through this
	// function's normal path below (patch/wait), which in turn waits on
	// the agent-driven Terminating-PodIOLimit handshake (line ~154) if
	// and when volumes drop to none -- the agent still resets while the
	// cgroup exists, exactly as required.
	//
	// What *does* change per F17: a pod already mid-termination (DeletionTimestamp
	// set, not yet terminal) that has no PodIOLimit yet must not get a
	// brand new one created for it below -- see the current == nil check
	// near the bottom of this function. That's pure waste (a full
	// create -> apply -> release round trip for a cgroup about to be
	// destroyed anyway), not a safety question: there is nothing to
	// release if nothing was ever created.
	podLive := podExists && pod.Spec.NodeName != "" && !podTerminal(pod.Status.Phase)

	// Step 1/2: separate the PodIOLimit matching the live pod's UID (if
	// any) from every other one, which by definition belongs to a pod
	// that's gone (recreated with a new UID, or the pod object itself no
	// longer exists). Those are released immediately: their cgroup is
	// gone, there's nothing for the agent to reset (D9).
	var current *storagev1alpha1.PodIOLimit
	for i := range pilList.Items {
		pil := &pilList.Items[i]
		if podExists && pil.Spec.PodUID == pod.UID {
			current = pil
			continue
		}
		if ok, err := r.releaseDirect(ctx, pil, "stale UID: pod recreated or gone"); err != nil {
			return ctrl.Result{}, err
		} else if !ok {
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
	}

	// Step 3: pod missing, unscheduled, or terminal -- desired is always
	// none, and if a PodIOLimit exists for this exact UID it's released
	// directly too (no live cgroup to hand off to the agent).
	if !podLive {
		if current == nil {
			return ctrl.Result{}, nil
		}
		reason := "pod missing"
		if podExists {
			reason = "pod terminal or not yet scheduled"
		}
		if ok, err := r.releaseDirect(ctx, current, reason); err != nil {
			return ctrl.Result{}, err
		} else if !ok {
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, nil
	}

	// D35: current (if any) is this pod's existing PodIOLimit -- its
	// Spec.Volumes is what an invalid limiter's contribution gets frozen
	// against, so an admin's typo can never loosen a limit already applied.
	var existingVolumes []storagev1alpha1.PodVolumeLimit
	if current != nil {
		existingVolumes = current.Spec.Volumes
	}
	// D37 update: limitersForPod adds back a limiter dropped only because
	// its own selector just went invalid, as long as it's still a source
	// in existingVolumes (still exists, selector just broke) -- otherwise
	// an InvalidSelector edit would silently loosen an applied limit.
	limiters, err := limitersForPod(ctx, r.Client, &pod, existingVolumes)
	if err != nil {
		return ctrl.Result{}, err
	}
	volumes, _ := desired.Compute(&pod, limiters, existingVolumes, pvcLookupFor(ctx, r.Client, pod.Namespace), pvLookupFor(ctx, r.Client))

	// A PodIOLimit still Terminating (D9's handshake in progress, or
	// waiting on the agent) must be left alone until the agent has
	// reset every device it owned -- regardless of what's newly desired.
	if current != nil && current.DeletionTimestamp != nil {
		if len(current.Status.Devices) == 0 {
			if ok, err := r.removeFinalizerOptimistic(ctx, current, "agent released (status.devices empty)"); err != nil {
				return ctrl.Result{}, err
			} else if !ok {
				return ctrl.Result{RequeueAfter: requeueSoon}, nil
			}
			return ctrl.Result{}, nil
		}
		log.V(1).Info("Waiting for Terminating PodIOLimit before recreating/patching",
			"podiolimit", current.Namespace+"/"+current.Name)
		return ctrl.Result{RequeueAfter: pilRequeueInterval}, nil
	}

	if len(volumes) == 0 {
		if current != nil {
			log.Info("Deleting PodIOLimit: no volumes to limit", "podiolimit", current.Namespace+"/"+current.Name)
			if err := r.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("deleting PodIOLimit: %w", err)
			}
			r.emitOnSources(ctx, pod.Namespace, sourcesOf(current.Spec.Volumes), corev1.EventTypeNormal,
				"PodIOLimitDeleted", fmt.Sprintf("PodIOLimit for pod %s/%s deleted: no volumes to limit", pod.Namespace, pod.Name))
		}
		return ctrl.Result{}, nil
	}

	if current == nil {
		// F17: don't create a PodIOLimit for a pod already mid graceful
		// termination -- its cgroup is on the way out, so a create here
		// would immediately be followed by a release with nothing gained.
		// This does not touch D9's release path: there is no PodIOLimit
		// to release in the first place.
		if pod.DeletionTimestamp != nil {
			log.V(1).Info("Skipping PodIOLimit creation: pod is already terminating")
			return ctrl.Result{}, nil
		}
		return r.createPodIOLimit(ctx, &pod, volumes)
	}

	return r.patchIfDiffer(ctx, current, volumes)
}

// getPod fetches key into pod via the cached client, falling back to one
// live, uncached read (r.APIReader) if the cache reports it missing (D30:
// avoids misidentifying a just-created pod not yet in the Pod informer's
// cache as "gone", which would otherwise wrongly release a legitimate
// PodIOLimit down the D9 dead-pod path). r.APIReader == nil skips the
// fallback (e.g. unit tests using only a fake client).
func (r *PodReconciler) getPod(ctx context.Context, key client.ObjectKey, pod *corev1.Pod) (bool, error) {
	err := r.Get(ctx, key, pod)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("getting pod: %w", err)
	}
	if err == nil {
		return true, nil
	}
	if r.APIReader == nil {
		return false, nil
	}
	liveErr := r.APIReader.Get(ctx, key, pod)
	if liveErr != nil && !apierrors.IsNotFound(liveErr) {
		return false, fmt.Errorf("getting pod (live read): %w", liveErr)
	}
	return liveErr == nil, nil
}

// releaseDirect deletes pil (if not already being deleted) and removes its
// finalizer without waiting on the agent: used when the pod it belongs to
// is definitely gone or terminal, so there is no live cgroup left to reset
// (D9's "dead" path, as opposed to the agent handshake).
func (r *PodReconciler) releaseDirect(ctx context.Context, pil *storagev1alpha1.PodIOLimit, reason string) (bool, error) {
	if pil.DeletionTimestamp == nil {
		if err := r.Delete(ctx, pil); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("deleting stale PodIOLimit: %w", err)
		}
	}
	return r.removeFinalizerOptimistic(ctx, pil, reason)
}

func (r *PodReconciler) createPodIOLimit(ctx context.Context, pod *corev1.Pod, volumes []storagev1alpha1.PodVolumeLimit) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pil := &storagev1alpha1.PodIOLimit{
		ObjectMeta: metav1.ObjectMeta{
			Name:            podIOLimitName(pod.Name, pod.UID),
			Namespace:       pod.Namespace,
			Labels:          map[string]string{managedByLabel: managedByValue},
			OwnerReferences: []metav1.OwnerReference{podOwnerRef(pod)},
			Finalizers:      []string{finalizerName},
		},
		Spec: storagev1alpha1.PodIOLimitSpec{
			NodeName: pod.Spec.NodeName,
			PodName:  pod.Name,
			PodUID:   pod.UID,
			QOSClass: pod.Status.QOSClass,
			Volumes:  volumes,
		},
	}

	if err := r.Create(ctx, pil); err != nil {
		// A concurrent reconcile for this exact pod already created this
		// object: not a real failure, just this reconcile's own cached
		// List-by-podName (Step 1/2, above) not yet reflecting a sibling
		// reconcile's very recent write (informer cache propagation lag --
		// observed on kind: the IOLimiter's own status write re-enqueues
		// this pod via mapIOLimiterToPods almost immediately after a
		// successful Create, before the cache catches up). Recording it as
		// PodIOLimitWriteFailed would be permanently wrong (nothing ever
		// clears it, since nothing about the object changes again to
		// re-trigger this reconcile) for something that isn't a failure at
		// all -- the object's own Create event already re-triggers a fresh
		// reconcile of this pod via the PodIOLimit watch (D30), which will
		// see and reconcile it normally.
		if apierrors.IsAlreadyExists(err) {
			log.V(1).Info("PodIOLimit already created by a concurrent reconcile, not a failure", "podiolimit", pil.Namespace+"/"+pil.Name)
			return ctrl.Result{}, nil
		}
		// F7: a Create failure (e.g. a VAP denial) must not only end up in
		// the controller's own logs via the returned error -- surface it
		// on every IOLimiter that would have contributed to this
		// PodIOLimit, as both a condition (visible on `kubectl get iol`)
		// and a Warning event. If recording that itself hits an
		// optimistic-lock conflict on the IOLimiter, requeue rather than
		// swallow it (D17/D26: a conflict is not an error) -- the next
		// attempt re-derives everything from scratch, so nothing besides
		// one extra round trip is lost.
		if r.recordPodIOLimitWriteFailure(ctx, pod.Namespace, sourcesOf(volumes), pod.Name, "PodIOLimitCreateFailed", err) {
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating PodIOLimit: %w", err)
	}
	if r.clearPodIOLimitWriteFailure(ctx, pod.Namespace, sourcesOf(volumes)) {
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}

	log.Info("PodIOLimit created", "podiolimit", pil.Namespace+"/"+pil.Name, "node", pil.Spec.NodeName,
		"volumes", volumeNames(volumes), "sources", sourcesOf(volumes), "finalizer", finalizerName)
	r.emitOnSources(ctx, pod.Namespace, sourcesOf(volumes), corev1.EventTypeNormal,
		"PodIOLimitCreated", fmt.Sprintf("PodIOLimit created for pod %s/%s", pod.Namespace, pod.Name))
	return ctrl.Result{}, nil
}

func (r *PodReconciler) patchIfDiffer(ctx context.Context, current *storagev1alpha1.PodIOLimit, volumes []storagev1alpha1.PodVolumeLimit) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	key := current.Namespace + "/" + current.Name

	added, removed, changed := diffVolumes(current.Spec.Volumes, volumes)
	if len(added) == 0 && len(removed) == 0 && len(changed) == 0 {
		log.V(1).Info("No-op reconcile: PodIOLimit spec unchanged", "podiolimit", key)
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.Spec.Volumes = volumes
	if err := r.Patch(ctx, current, patch); err != nil {
		// F7: same surfacing as the Create path above, for a Patch denial,
		// with the same conflict-requeues-instead-of-swallows treatment.
		if r.recordPodIOLimitWriteFailure(ctx, current.Namespace, sourcesOf(volumes), current.Spec.PodName, "PodIOLimitPatchFailed", err) {
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, fmt.Errorf("patching PodIOLimit: %w", err)
	}
	if r.clearPodIOLimitWriteFailure(ctx, current.Namespace, sourcesOf(volumes)) {
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}

	log.Info("PodIOLimit patched", "podiolimit", key, "volumesAdded", added, "volumesRemoved", removed,
		"volumesChanged", changed, "sources", sourcesOf(volumes))
	return ctrl.Result{}, nil
}

// recordPodIOLimitWriteFailure implements F7's controller-half requirement:
// a Create/Patch failure PodReconciler hits (e.g. a VAP denial) must be
// visible on the IOLimiter(s) that would have contributed to the write, not
// only in the controller's own logs. It sets conditionPodIOLimitWriteFailed
// (defined in iolimiter_controller.go; IOLimiterReconciler folds it into
// the aggregated Ready state and never itself touches a condition of this
// Type, so it survives untouched across that reconciler's own status
// writes) and emits a matching Warning event. A limiter that no longer
// exists, or one whose condition already carries the identical message
// (steady retries of the same denial), is skipped to avoid writing on
// every reconcile.
// Returns true if the Status().Update on any touched IOLimiter hit an
// optimistic-lock conflict (D17/D26: a conflict is not an error, but the
// caller must requeue rather than let the stale condition stick -- see the
// callers in createPodIOLimit/patchIfDiffer).
func (r *PodReconciler) recordPodIOLimitWriteFailure(ctx context.Context, namespace string, sources []string, podName, reason string, cause error) (conflict bool) {
	log := logf.FromContext(ctx)
	message := fmt.Sprintf("pod %s: %s: %s", podName, reason, cause)
	for _, name := range sources {
		var l storagev1alpha1.IOLimiter
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &l); err != nil {
			log.V(1).Info("Skipping write-failure record on IOLimiter, not found", "iolimiter", namespace+"/"+name)
			continue
		}
		if existing := meta.FindStatusCondition(l.Status.Conditions, conditionPodIOLimitWriteFailed); existing != nil &&
			existing.Status == metav1.ConditionTrue && existing.Message == message {
			continue
		}
		meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{
			Type: conditionPodIOLimitWriteFailed, Status: metav1.ConditionTrue,
			ObservedGeneration: l.Generation, Reason: reason, Message: message,
		})
		if err := r.Status().Update(ctx, &l); err != nil {
			if apierrors.IsConflict(err) {
				log.V(1).Info("Status update conflict recording PodIOLimit write failure, requeueing",
					"iolimiter", namespace+"/"+name)
				conflict = true
				continue
			}
			log.V(1).Info("Failed to record PodIOLimit write failure on IOLimiter",
				"iolimiter", namespace+"/"+name, "error", err.Error())
			continue
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(&l, nil, corev1.EventTypeWarning, reason, reason, "%s", message)
		}
	}
	return conflict
}

// clearPodIOLimitWriteFailure clears conditionPodIOLimitWriteFailed on
// every named IOLimiter once a Create/Patch that previously failed for it
// succeeds. A limiter with no such condition (the common case: nothing
// ever failed) is left untouched -- no write, no event.
// Returns true if the Status().Update on any touched IOLimiter hit an
// optimistic-lock conflict, same treatment as recordPodIOLimitWriteFailure.
func (r *PodReconciler) clearPodIOLimitWriteFailure(ctx context.Context, namespace string, sources []string) (conflict bool) {
	log := logf.FromContext(ctx)
	for _, name := range sources {
		var l storagev1alpha1.IOLimiter
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &l); err != nil {
			continue
		}
		existing := meta.FindStatusCondition(l.Status.Conditions, conditionPodIOLimitWriteFailed)
		if existing == nil || existing.Status != metav1.ConditionTrue {
			continue
		}
		meta.SetStatusCondition(&l.Status.Conditions, metav1.Condition{
			Type: conditionPodIOLimitWriteFailed, Status: metav1.ConditionFalse,
			ObservedGeneration: l.Generation, Reason: "Recovered", Message: "",
		})
		if err := r.Status().Update(ctx, &l); err != nil {
			if apierrors.IsConflict(err) {
				log.V(1).Info("Status update conflict clearing PodIOLimit write failure, requeueing",
					"iolimiter", namespace+"/"+name)
				conflict = true
				continue
			}
			log.V(1).Info("Failed to clear PodIOLimit write failure on IOLimiter",
				"iolimiter", namespace+"/"+name, "error", err.Error())
		}
	}
	return conflict
}

// emitOnSources records an event on every named IOLimiter (D26: create/
// delete transitions are IOLimiter events, attributed to the limiters that
// caused them). A limiter that no longer exists is skipped (V(1)): it
// can't be the reason a human is watching `kubectl describe iolimiter` on
// it.
func (r *PodReconciler) emitOnSources(ctx context.Context, namespace string, sources []string, eventtype, reason, message string) {
	if r.Recorder == nil {
		return
	}
	log := logf.FromContext(ctx)
	for _, name := range sources {
		var l storagev1alpha1.IOLimiter
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &l); err != nil {
			log.V(1).Info("Skipping event on IOLimiter, not found", "iolimiter", namespace+"/"+name)
			continue
		}
		r.Recorder.Eventf(&l, nil, eventtype, reason, reason, "%s", message)
	}
}

// podIOLimitName implements D18: "<podName>-<podUID[:8]>", truncating
// podName so the whole name stays <= 253 characters.
func podIOLimitName(podName string, uid types.UID) string {
	suffix := string(uid)
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	name := podName
	maxNameLen := max(253-len(suffix)-1, 0)
	if len(name) > maxNameLen {
		name = name[:maxNameLen]
	}
	return name + "-" + suffix
}

// podOwnerRef builds the ownerRef D18 requires: controller: true (so GC
// treats the pod as this object's owning controller),
// blockOwnerDeletion: false (deleting the pod must never be blocked
// waiting on this object -- the finalizer, not an owner-deletion block,
// is what sequences the agent's reset, D9).
func podOwnerRef(pod *corev1.Pod) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Pod",
		Name:               pod.Name,
		UID:                pod.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(false),
	}
}

func volumeNames(volumes []storagev1alpha1.PodVolumeLimit) []string {
	out := make([]string, 0, len(volumes))
	for _, v := range volumes {
		out = append(out, v.Name)
	}
	return out
}

func sourcesOf(volumes []storagev1alpha1.PodVolumeLimit) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, v := range volumes {
		for _, s := range v.Sources {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// diffVolumes reports which volume names were added, removed, or changed
// (limits or sources differ) between old and new -- the D26 diff summary
// on every PodIOLimit patch.
func diffVolumes(oldVols, newVols []storagev1alpha1.PodVolumeLimit) (added, removed, changed []string) {
	oldByName := make(map[string]storagev1alpha1.PodVolumeLimit, len(oldVols))
	for _, v := range oldVols {
		oldByName[v.Name] = v
	}
	newByName := make(map[string]storagev1alpha1.PodVolumeLimit, len(newVols))
	for _, v := range newVols {
		newByName[v.Name] = v
	}

	for name, nv := range newByName {
		ov, ok := oldByName[name]
		if !ok {
			added = append(added, name)
			continue
		}
		if !volumeLimitEqual(ov, nv) {
			changed = append(changed, name)
		}
	}
	for name := range oldByName {
		if _, ok := newByName[name]; !ok {
			removed = append(removed, name)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(changed)
	return added, removed, changed
}

func volumeLimitEqual(a, b storagev1alpha1.PodVolumeLimit) bool {
	if a.KubeletDirName != b.KubeletDirName {
		return false
	}
	if !deviceLimitsEqual(a.Limits, b.Limits) {
		return false
	}
	if len(a.Sources) != len(b.Sources) {
		return false
	}
	for i := range a.Sources {
		if a.Sources[i] != b.Sources[i] {
			return false
		}
	}
	// D36: two volumes can have equal merged Limits/Sources while a
	// per-limiter Contribution still moved underneath (e.g. one limiter's
	// own field changed but wasn't the binding minimum) -- that's still a
	// spec change worth patching and logging, so compare it too.
	if len(a.Contributions) != len(b.Contributions) {
		return false
	}
	for i := range a.Contributions {
		if a.Contributions[i].Source != b.Contributions[i].Source ||
			!deviceLimitsEqual(a.Contributions[i].Limits, b.Contributions[i].Limits) {
			return false
		}
	}
	return true
}

func deviceLimitsEqual(a, b storagev1alpha1.DeviceLimits) bool {
	return int64PtrEqual(a.ReadBPS, b.ReadBPS) &&
		int64PtrEqual(a.WriteBPS, b.WriteBPS) &&
		int64PtrEqual(a.ReadIOPS, b.ReadIOPS) &&
		int64PtrEqual(a.WriteIOPS, b.WriteIOPS)
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// mapIOLimiterToPods enqueues every pod matching the IOLimiter's selector,
// plus every pod whose PodIOLimit already lists this limiter in sources
// (so an edit that narrows or removes a limiter still triggers
// recomputation for pods it used to affect -- D10, no finalizer on
// IOLimiter, the PodIOLimit's own sources are the source of truth for who
// to revisit).
func mapIOLimiterToPods(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []ctrl.Request {
		limiter, ok := obj.(*storagev1alpha1.IOLimiter)
		if !ok {
			return nil
		}
		seen := map[types.NamespacedName]struct{}{}
		var reqs []ctrl.Request
		add := func(ns, name string) {
			key := types.NamespacedName{Namespace: ns, Name: name}
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			reqs = append(reqs, ctrl.Request{NamespacedName: key})
		}

		sel, err := globalSelectorCache.selectorFor(limiter)
		if err == nil {
			var pods corev1.PodList
			if err := c.List(ctx, &pods, client.InNamespace(limiter.Namespace)); err == nil {
				for i := range pods.Items {
					if sel.Matches(labels.Set(pods.Items[i].Labels)) {
						add(pods.Items[i].Namespace, pods.Items[i].Name)
					}
				}
			}
		}

		var pils storagev1alpha1.PodIOLimitList
		if err := c.List(ctx, &pils, client.InNamespace(limiter.Namespace),
			client.MatchingFields{indexPodIOLimitBySource: limiter.Name}); err == nil {
			for i := range pils.Items {
				add(pils.Items[i].Namespace, pils.Items[i].Spec.PodName)
			}
		}

		return reqs
	}
}

// mapPodIOLimitToPod enqueues the pod named by a PodIOLimit's own
// spec.podName, regardless of ownerRef (D30). This is what lets an orphan
// PodIOLimit -- one with no ownerRef at all, or one whose ownerRef points
// at a pod that's since been recreated/deleted -- reach Reconcile: relying
// on Owns() alone (ownerRef-driven) never enqueues an object the
// controller didn't itself create with an ownerRef, which is exactly the
// shape of the reported orphan (a leftover with no ownerRef, pointing at a
// nonexistent pod UID). Every PodIOLimit's own create/update event
// (including the synthetic Create controller-runtime's cache emits for
// every object already present when a watch (re)syncs, e.g. on controller
// restart) is enough to trigger Reconcile's existing D9 dead-pod path.
func mapPodIOLimitToPod() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []ctrl.Request {
		pil, ok := obj.(*storagev1alpha1.PodIOLimit)
		if !ok || pil.Spec.PodName == "" {
			return nil
		}
		return []ctrl.Request{{NamespacedName: types.NamespacedName{
			Namespace: pil.Namespace,
			Name:      pil.Spec.PodName,
		}}}
	}
}

// mapPVCToPods enqueues every pod that references the changed PVC (direct
// or generic-ephemeral), via the PVC-claim-name pod index (D16).
func mapPVCToPods(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []ctrl.Request {
		pvc, ok := obj.(*corev1.PersistentVolumeClaim)
		if !ok {
			return nil
		}
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(pvc.Namespace),
			client.MatchingFields{indexPodByPVCClaimName: pvc.Name}); err != nil {
			return nil
		}
		reqs := make([]ctrl.Request, 0, len(pods.Items))
		for i := range pods.Items {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pods.Items[i])})
		}
		return reqs
	}
}

// mapPVToPods enqueues every pod bound (via its PVC) to the changed
// PersistentVolume (F4/F9): a PV's own claim binding (spec.claimRef) names
// the PVC it's bound to, and the existing PVC-claim-name pod index (D16)
// finds the pods that reference that PVC. This is what makes a PV Bound
// transition -- or, per F4, discovering it's hostPath-backed -- promptly
// re-run desired.Compute for the pods waiting on it, instead of only ever
// being noticed on the next unrelated reconcile.
func mapPVToPods(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []ctrl.Request {
		pv, ok := obj.(*corev1.PersistentVolume)
		if !ok || pv.Spec.ClaimRef == nil {
			return nil
		}
		var pods corev1.PodList
		if err := c.List(ctx, &pods, client.InNamespace(pv.Spec.ClaimRef.Namespace),
			client.MatchingFields{indexPodByPVCClaimName: pv.Spec.ClaimRef.Name}); err != nil {
			return nil
		}
		reqs := make([]ctrl.Request, 0, len(pods.Items))
		for i := range pods.Items {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pods.Items[i])})
		}
		return reqs
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}, builder.WithPredicates(podRelevantChangePredicate())).
		Watches(&storagev1alpha1.IOLimiter{}, handler.EnqueueRequestsFromMapFunc(mapIOLimiterToPods(mgr.GetClient()))).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(mapPVCToPods(mgr.GetClient())),
			builder.WithPredicates(pvcRelevantChangePredicate())).
		// F4/F9: a PV's hostPath-ness (or its Bound transition) matters to
		// desired.Compute the same way a PVC's does.
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(mapPVToPods(mgr.GetClient())),
			builder.WithPredicates(pvRelevantChangePredicate())).
		// D30: mapped by spec.podName, not Owns() -- Owns() only fires for
		// objects with a matching ownerRef, which an orphan PodIOLimit (no
		// ownerRef, or one pointing at a dead pod) by definition doesn't
		// have.
		Watches(&storagev1alpha1.PodIOLimit{}, handler.EnqueueRequestsFromMapFunc(mapPodIOLimitToPod()),
			builder.WithPredicates(podIOLimitRelevantChangePredicate())).
		Named("pod").
		Complete(r)
}
