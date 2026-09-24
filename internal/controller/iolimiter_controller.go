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
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/controller/desired"
)

// maxFailingPodsInMessage bounds the Ready condition's message (D20: "The
// message names up to 5 failing pods"), applied separately to each of the
// failed/note/pending entry groups built in Reconcile.
const maxFailingPodsInMessage = 5

// conditionPodIOLimitWriteFailed is set directly by PodReconciler (F7, not
// via desired.Compute: a Create/Patch failure -- e.g. a VAP denial -- is a
// controller-side write failure, not one of desired.Compute's pure
// (pod, limiters) => issues problems, so it can't flow through the shared
// core both reconcilers already use). IOLimiterReconciler folds it into
// the aggregated Ready state and message without erasing it: it only ever
// touches the "Ready" condition itself (meta.SetStatusCondition), so this
// condition -- set on the same object by the other reconciler -- survives
// untouched across every IOLimiterReconciler status write.
const conditionPodIOLimitWriteFailed = "PodIOLimitWriteFailed"

// IOLimiterReconciler reconciles an IOLimiter object, implementing
// SPEC.md §6.2's IOLimiterReconciler algorithm: it aggregates the
// controller-side issues (from the same desired.Compute PodReconciler
// uses) and every matched pod's PodIOLimit volume states into
// IOLimiter.status (D20).
type IOLimiterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits events on the IOLimiter itself (D26): UnsupportedVolume
	// / VolumeNotFound / PVCNotBound on first appearance, Ready / NotReady
	// on transition.
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=storage.maurice.fr,resources=iolimiters,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.maurice.fr,resources=iolimiters/status,verbs=get;update;patch

// Reconcile implements SPEC.md §6.2's IOLimiterReconciler.
func (r *IOLimiterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("iolimiter", req.Namespace+"/"+req.Name)
	ctx = logf.IntoContext(ctx, log)

	var cr storagev1alpha1.IOLimiter
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			deleteIOLimiterPodsGauge(req.Namespace + "/" + req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Snapshot by value, not by pointer: meta.SetStatusCondition (called
	// below via setIOLimiterReady) mutates an existing "Ready" condition
	// struct in place rather than replacing it, so a pointer captured here
	// would silently alias the post-update value by the time it's compared
	// against newReady -- every transition after the very first would
	// compare a condition against itself and never log or emit an event
	// (found while writing the e2e IOLimiter Ready-transition/event
	// assertions, C5;
	// TestIOLimiterReconciler_ReadyTransition_SecondReconcileLogsAndEmits
	// regression-tests it directly).
	oldReady := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
	if oldReady != nil {
		snapshot := *oldReady
		oldReady = &snapshot
	}
	oldMessage := ""
	if oldReady != nil {
		oldMessage = oldReady.Message
	}
	// F9: a full deep copy of the pre-reconcile status, compared against
	// the freshly computed one below so a semantically-unchanged reconcile
	// (D26's "status is written only when it semantically changed" applied
	// to the controller side, mirroring the agent's own
	// statusSemanticallyEqual) skips the Status().Update entirely --
	// otherwise every reconcile pays an API write even when nothing an
	// operator cares about moved.
	oldStatus := *cr.Status.DeepCopy()

	// D37: bounded, cached selector parse. A selector that exceeds the
	// bound is reported as InvalidSelector and contributes nothing (no
	// pods are even listed) rather than retried forever as an error --
	// this is also the DoS defense itself: the bound is checked before
	// building a labels.Selector at all, so a hostile selector never
	// reaches per-pod matching.
	sel, err := globalSelectorCache.selectorFor(&cr)
	if err != nil {
		return r.reconcileInvalidSelector(ctx, &cr, oldReady, oldMessage, oldStatus, err)
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cr.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing pods: %w", err)
	}

	tally := &podEvalTally{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || podTerminal(pod.Status.Phase) {
			continue
		}
		if !sel.Matches(labels.Set(pod.Labels)) {
			continue
		}
		if err := r.evalPod(ctx, pod, &cr, oldMessage, tally); err != nil {
			return ctrl.Result{}, err
		}
	}

	// F7: a Create/Patch failure PodReconciler couldn't even get a
	// PodIOLimit written for (e.g. a VAP denial) would otherwise show up
	// here as silent Pending (no PodIOLimit exists to inspect) -- fold the
	// condition PodReconciler set directly on this object into the
	// aggregated Failed state so it's visible on `kubectl get iol`, not
	// only in logs.
	if writeFailed := meta.FindStatusCondition(cr.Status.Conditions, conditionPodIOLimitWriteFailed); writeFailed != nil && writeFailed.Status == metav1.ConditionTrue {
		tally.failedPods++
		tally.failEntries = append(tally.failEntries, writeFailed.Message)
	}

	slices.Sort(tally.failEntries)
	slices.Sort(tally.noteEntries)
	slices.Sort(tally.pendingEntries)
	slices.Sort(tally.withoutVolumesEntries)
	setIOLimiterReady(&cr, tally.matchedPods, tally.appliedPods, tally.failedPods,
		tally.failEntries, tally.noteEntries, tally.pendingEntries, tally.withoutVolumesEntries)
	cr.Status.MatchedPods = tally.matchedPods
	cr.Status.AppliedPods = tally.appliedPods
	cr.Status.FailedPods = tally.failedPods
	cr.Status.SelectedPodsWithoutVolumes = int32(len(tally.withoutVolumesEntries))
	cr.Status.ObservedGeneration = cr.Generation
	setIOLimiterPodsGauge(cr.Namespace+"/"+cr.Name, tally.matchedPods, tally.appliedPods, tally.failedPods, tally.pendingPods)

	if iolimiterStatusEqual(oldStatus, cr.Status) {
		log.V(1).Info("Status unchanged, not written")
		return ctrl.Result{}, nil
	}

	if err := r.Status().Update(ctx, &cr); err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("Status update conflict, requeueing")
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, fmt.Errorf("updating IOLimiter status: %w", err)
	}

	newReady := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
	if oldReady == nil || oldReady.Status != newReady.Status || oldReady.Reason != newReady.Reason {
		log.Info("IOLimiter Ready transition",
			"from", conditionReasonOf(oldReady), "to", newReady.Reason,
			"matchedPods", tally.matchedPods, "appliedPods", tally.appliedPods, "failedPods", tally.failedPods,
			"observedGeneration", cr.Status.ObservedGeneration)
		r.emitReadyEvent(&cr, newReady)
	}
	for _, msg := range tally.newWithoutVolumesEvents {
		log.Info("Controller-side issue", "reason", "SelectedPodWithoutVolumes", "message", msg)
		r.event(&cr, corev1.EventTypeWarning, "SelectedPodWithoutVolumes", msg)
	}
	for _, w := range tally.newWarnings {
		log.Info("Controller-side issue", "volume", w.Volume, "reason", w.Reason, "message", w.Message)
		r.event(&cr, corev1.EventTypeWarning, w.Reason, fmt.Sprintf("volume %s: %s", w.Volume, w.Message))
	}

	return ctrl.Result{}, nil
}

// podEvalTally accumulates evalPod's per-pod results across one Reconcile
// call: extracted from Reconcile's own loop body purely to keep its
// cyclomatic complexity within gocyclo's 30 limit (mirrors the same move
// PodReconciler/computeVolumesFor already made).
type podEvalTally struct {
	matchedPods, appliedPods, failedPods, pendingPods int32
	failEntries, noteEntries, pendingEntries          []string
	withoutVolumesEntries                             []string
	newWarnings                                       []desired.Issue
	newWithoutVolumesEvents                           []string
}

// evalPod implements one selector-matched pod's contribution to
// Reconcile's aggregate status (D20/D31/D38): whether it counts as matched,
// applied, failed, pending, or selected-without-volumes, and which new
// warning events (if any) that state change is worth emitting. oldMessage
// is the previous Ready message, used for D26's "only emit once per new
// message" event dedup.
func (r *IOLimiterReconciler) evalPod(ctx context.Context, pod *corev1.Pod, cr *storagev1alpha1.IOLimiter, oldMessage string, tally *podEvalTally) error {
	if !podHasAnyListedVolume(pod, cr) {
		// D38: selected by podSelector, but none of the listed volumes
		// exist on this pod -- an evasion (renamed volume/relabeled pod),
		// surfaced rather than silently indistinguishable from "nothing
		// selects this pod at all".
		entry := fmt.Sprintf("%s: SelectedPodWithoutVolumes: pod matches podSelector but has none of the listed volumes", pod.Name)
		tally.withoutVolumesEntries = append(tally.withoutVolumesEntries, entry)
		if !strings.Contains(oldMessage, entry) {
			tally.newWithoutVolumesEvents = append(tally.newWithoutVolumesEvents, entry)
		}
		return nil
	}
	tally.matchedPods++

	volumes, issues, err := r.computeVolumesFor(ctx, pod)
	if err != nil {
		return err
	}

	myIssues := issuesFor(issues, cr.Name)
	blocking, notFound := splitVolumeNotFound(myIssues)
	// D31/F11: a volume this limiter lists but the pod doesn't have is a
	// note, not a failure -- a limiter covering "data"+"wal" must not fail
	// a pod that only has "data" and whose "data" rule is live.
	for _, is := range notFound {
		entry := fmt.Sprintf("%s: volume %s: VolumeNotFound: %s", pod.Name, is.Volume, is.Message)
		tally.noteEntries = append(tally.noteEntries, entry)
		if !strings.Contains(oldMessage, entry) {
			tally.newWarnings = append(tally.newWarnings, is)
		}
	}
	if len(blocking) > 0 {
		tally.failedPods++
		for _, is := range blocking {
			entry := fmt.Sprintf("%s: volume %s: %s: %s", pod.Name, is.Volume, is.Reason, is.Message)
			tally.failEntries = append(tally.failEntries, entry)
			if !strings.Contains(oldMessage, entry) {
				tally.newWarnings = append(tally.newWarnings, is)
			}
		}
		return nil
	}

	state, detail := r.podIOLimitStateFor(ctx, pod, cr, volumes)
	switch state {
	case volumeStateApplied:
		tally.appliedPods++
	case volumeStateFailed:
		tally.failedPods++
		tally.failEntries = append(tally.failEntries, fmt.Sprintf("%s: %s", pod.Name, detail))
	default: // Pending
		tally.pendingPods++
		if detail != "" {
			tally.pendingEntries = append(tally.pendingEntries, fmt.Sprintf("%s: %s", pod.Name, detail))
		}
	}
	return nil
}

// reconcileInvalidSelector implements D37's fallback: a podSelector that
// exceeds the bound (see PodSelector's own doc comment for why this is a
// controller-side check, not CEL) is reported as Ready=False/InvalidSelector
// and contributes to no pods at all -- pods are never even listed, so a
// hostile selector can't burn CPU on per-pod matching either. Mirrors the
// main Reconcile's own status-write/event-dedup conventions (F9 semantic
// skip, D26 event-once-per-message).
func (r *IOLimiterReconciler) reconcileInvalidSelector(ctx context.Context, cr *storagev1alpha1.IOLimiter, oldReady *metav1.Condition, oldMessage string, oldStatus storagev1alpha1.IOLimiterStatus, selErr error) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	message := fmt.Sprintf("podSelector: %s", selErr)

	cr.Status.MatchedPods = 0
	cr.Status.AppliedPods = 0
	cr.Status.FailedPods = 0
	cr.Status.SelectedPodsWithoutVolumes = 0
	cr.Status.ObservedGeneration = cr.Generation
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: cr.Generation,
		Reason: "InvalidSelector", Message: message,
	})
	setIOLimiterPodsGauge(cr.Namespace+"/"+cr.Name, 0, 0, 0, 0)

	if iolimiterStatusEqual(oldStatus, cr.Status) {
		log.V(1).Info("Status unchanged, not written")
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("Status update conflict, requeueing")
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, fmt.Errorf("updating IOLimiter status: %w", err)
	}

	newReady := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
	if oldReady == nil || oldReady.Status != newReady.Status || oldReady.Reason != newReady.Reason {
		log.Info("IOLimiter Ready transition", "from", conditionReasonOf(oldReady), "to", newReady.Reason,
			"observedGeneration", cr.Status.ObservedGeneration)
		r.emitReadyEvent(cr, newReady)
	}
	if !strings.Contains(oldMessage, message) {
		r.event(cr, corev1.EventTypeWarning, "InvalidSelector", message)
	}
	return ctrl.Result{}, nil
}

type volumeState int

const (
	volumeStatePending volumeState = iota
	volumeStateApplied
	volumeStateFailed
)

// podIOLimitStateFor looks at pod's PodIOLimit (if any) and aggregates the
// state of every volume cr contributes to (D20's node-side half): Applied
// only if every such volume is Applied, Failed if any is Failed/
// Unsupported, Pending otherwise (including no PodIOLimit yet, or a stale
// observedGeneration -- an agent that's down leaves pods visibly Pending,
// D20's explicit requirement).
// podIOLimitStateFor's second return value carries a human-readable detail
// for whichever state it returns (F8: Pending is no longer opaque --
// "pod X volume Y: Pending: <agent reason/message>" is exactly what the
// agent's own PodIOLimit.status.volumes[] said, surfaced up to the
// IOLimiter so `kubectl describe iolimiter` alone answers "why is this
// pending").
func (r *IOLimiterReconciler) podIOLimitStateFor(ctx context.Context, pod *corev1.Pod, cr *storagev1alpha1.IOLimiter, volumes []storagev1alpha1.PodVolumeLimit) (volumeState, string) {
	var myVolumeNames []string
	for _, v := range volumes {
		if slices.Contains(v.Sources, cr.Name) {
			myVolumeNames = append(myVolumeNames, v.Name)
		}
	}
	if len(myVolumeNames) == 0 {
		// cr named a volume but contributed nothing resolvable to it
		// (e.g. an InvalidLimit-only field): no node-side state to check.
		return volumeStatePending, ""
	}

	var pilList storagev1alpha1.PodIOLimitList
	if err := r.List(ctx, &pilList, client.InNamespace(pod.Namespace),
		client.MatchingFields{indexPodIOLimitByPodName: pod.Name}); err != nil {
		return volumeStatePending, ""
	}
	var pil *storagev1alpha1.PodIOLimit
	for i := range pilList.Items {
		if pilList.Items[i].Spec.PodUID == pod.UID {
			pil = &pilList.Items[i]
			break
		}
	}
	if pil == nil {
		return volumeStatePending, fmt.Sprintf("volume %s: Pending: PodIOLimit not created yet", myVolumeNames[0])
	}
	if pil.Status.ObservedGeneration != pil.Generation {
		return volumeStatePending, fmt.Sprintf("volume %s: Pending: waiting for the agent to observe the current spec", myVolumeNames[0])
	}

	state := volumeStateApplied
	var detail string
	for _, name := range myVolumeNames {
		vs := findVolumeStatus(pil, name)
		switch {
		case vs == nil:
			if state != volumeStateFailed {
				state = volumeStatePending
				detail = fmt.Sprintf("volume %s: Pending: not yet reconciled by the agent", name)
			}
		case vs.State == "Applied":
			// ok
		case vs.State == "Failed" || vs.State == "Unsupported":
			state = volumeStateFailed
			detail = fmt.Sprintf("volume %s: %s: %s", vs.Name, vs.Reason, vs.Message)
		default: // Pending
			if state != volumeStateFailed {
				state = volumeStatePending
				detail = fmt.Sprintf("volume %s: Pending: %s: %s", vs.Name, vs.Reason, vs.Message)
			}
		}
	}
	return state, detail
}

// computeVolumesFor looks up pod's matching limiters and its existing
// PodIOLimit (D35's freeze-don't-loosen input), then runs desired.Compute --
// factored out of Reconcile purely to keep its own cyclomatic complexity
// down. Must agree with what PodReconciler itself computes, or this status
// view and the actual applied spec would disagree about what "matched"
// means.
func (r *IOLimiterReconciler) computeVolumesFor(ctx context.Context, pod *corev1.Pod) ([]storagev1alpha1.PodVolumeLimit, []desired.Issue, error) {
	existingVolumes, err := existingPodIOLimitVolumes(ctx, r.Client, pod)
	if err != nil {
		return nil, nil, err
	}
	// D37 update: see PodReconciler's identical use of limitersForPod --
	// this status view must agree with what PodReconciler actually wrote.
	limiters, err := limitersForPod(ctx, r.Client, pod, existingVolumes)
	if err != nil {
		return nil, nil, err
	}
	volumes, issues := desired.Compute(pod, limiters, existingVolumes, pvcLookupFor(ctx, r.Client, pod.Namespace), pvLookupFor(ctx, r.Client))
	return volumes, issues, nil
}

// existingPodIOLimitVolumes returns pod's current PodIOLimit's
// Spec.Volumes (nil if none exists yet), for D35's freeze-don't-loosen
// input to desired.Compute. Mirrors PodReconciler's own "list by podName
// index, match by UID" lookup (pod_controller.go's Reconcile steps 1/2) --
// deliberately a small local duplication rather than a shared helper, since
// that one also does release/orphan handling this reconciler has no
// business touching.
func existingPodIOLimitVolumes(ctx context.Context, c client.Client, pod *corev1.Pod) ([]storagev1alpha1.PodVolumeLimit, error) {
	var pilList storagev1alpha1.PodIOLimitList
	if err := c.List(ctx, &pilList, client.InNamespace(pod.Namespace),
		client.MatchingFields{indexPodIOLimitByPodName: pod.Name}); err != nil {
		return nil, fmt.Errorf("listing PodIOLimits for pod: %w", err)
	}
	for i := range pilList.Items {
		if pilList.Items[i].Spec.PodUID == pod.UID {
			return pilList.Items[i].Spec.Volumes, nil
		}
	}
	return nil, nil
}

func findVolumeStatus(pil *storagev1alpha1.PodIOLimit, name string) *storagev1alpha1.VolumeStatus {
	for i := range pil.Status.Volumes {
		if pil.Status.Volumes[i].Name == name {
			return &pil.Status.Volumes[i]
		}
	}
	return nil
}

func issuesFor(issues []desired.Issue, limiterName string) []desired.Issue {
	var out []desired.Issue
	for _, is := range issues {
		if is.LimiterName == limiterName {
			out = append(out, is)
		}
	}
	return out
}

// splitVolumeNotFound separates D31's warning-only VolumeNotFound issues
// (a listed volume the pod simply doesn't have) from every other
// controller-side issue, which still fails the pod outright (D20).
func splitVolumeNotFound(issues []desired.Issue) (blocking, notFound []desired.Issue) {
	for _, is := range issues {
		if is.Reason == desired.ReasonVolumeNotFound {
			notFound = append(notFound, is)
		} else {
			blocking = append(blocking, is)
		}
	}
	return blocking, notFound
}

// podHasAnyListedVolume implements the matchedPods definition (D20/§6.2):
// a pod counts as matched only if it actually has at least one of cr's
// listed volume names, not merely because it matches the selector.
func podHasAnyListedVolume(pod *corev1.Pod, cr *storagev1alpha1.IOLimiter) bool {
	for _, vl := range cr.Spec.Volumes {
		for _, pv := range pod.Spec.Volumes {
			if pv.Name == vl.Name {
				return true
			}
		}
	}
	return false
}

// setIOLimiterReady sets the Ready condition per D20/§4.1's documented
// reasons. The message names up to 5 failing pods, plus (D31/F11) up to 5
// VolumeNotFound notes, (F8) up to 5 Pending explanations, and (D38) up to
// 5 SelectedPodWithoutVolumes notes, so Ready stays opaque about at most
// the counts, never about the reasons. D38 does not change status/reason
// here: a selected pod without volumes is neither a failure nor pending,
// only visible in the message and selectedPodsWithoutVolumes.
func setIOLimiterReady(cr *storagev1alpha1.IOLimiter, matched, applied, failed int32, failEntries, noteEntries, pendingEntries, withoutVolumesEntries []string) {
	status := metav1.ConditionFalse
	var reason string

	switch {
	case matched == 0:
		status = metav1.ConditionTrue
		reason = "NoMatchingPods"
	case failed == 0 && applied == matched:
		status = metav1.ConditionTrue
		reason = "AllApplied"
	case failed > 0:
		reason = "Failed"
	default:
		reason = "Pending"
	}

	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            buildReadyMessage(failEntries, noteEntries, pendingEntries, withoutVolumesEntries),
	})
}

// buildReadyMessage joins up to maxFailingPodsInMessage entries from each
// of failEntries, noteEntries, pendingEntries and withoutVolumesEntries (in
// that order), each group capped independently so a limiter with many
// pending pods doesn't crowd out its failures or vice versa.
func buildReadyMessage(failEntries, noteEntries, pendingEntries, withoutVolumesEntries []string) string {
	parts := make([]string, 0, min(len(failEntries), maxFailingPodsInMessage)+
		min(len(noteEntries), maxFailingPodsInMessage)+min(len(pendingEntries), maxFailingPodsInMessage)+
		min(len(withoutVolumesEntries), maxFailingPodsInMessage))
	parts = append(parts, capEntries(failEntries)...)
	parts = append(parts, capEntries(noteEntries)...)
	parts = append(parts, capEntries(pendingEntries)...)
	parts = append(parts, capEntries(withoutVolumesEntries)...)
	return strings.Join(parts, "; ")
}

func capEntries(entries []string) []string {
	if len(entries) > maxFailingPodsInMessage {
		return entries[:maxFailingPodsInMessage]
	}
	return entries
}

// iolimiterStatusEqual reports whether a and b are semantically the same
// status (F9: skip the write, mirroring the agent's own
// statusSemanticallyEqual convention), ignoring each condition's
// LastTransitionTime -- meta.SetStatusCondition bumps that on every call
// regardless of whether anything else moved, so comparing it verbatim
// would defeat the whole point of this check.
func iolimiterStatusEqual(a, b storagev1alpha1.IOLimiterStatus) bool {
	if a.MatchedPods != b.MatchedPods || a.AppliedPods != b.AppliedPods ||
		a.FailedPods != b.FailedPods || a.SelectedPodsWithoutVolumes != b.SelectedPodsWithoutVolumes ||
		a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type || ac.Status != bc.Status || ac.Reason != bc.Reason ||
			ac.Message != bc.Message || ac.ObservedGeneration != bc.ObservedGeneration {
			return false
		}
	}
	return true
}

func conditionReasonOf(c *metav1.Condition) string {
	if c == nil {
		return "<none>"
	}
	return c.Reason
}

func (r *IOLimiterReconciler) event(cr *storagev1alpha1.IOLimiter, eventtype, reason, message string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(cr, nil, eventtype, reason, reason, "%s", message)
}

func (r *IOLimiterReconciler) emitReadyEvent(cr *storagev1alpha1.IOLimiter, ready *metav1.Condition) {
	if ready == nil {
		return
	}
	if ready.Status == metav1.ConditionTrue {
		r.event(cr, corev1.EventTypeNormal, "Ready", ready.Message)
		return
	}
	r.event(cr, corev1.EventTypeWarning, "NotReady", ready.Message)
}

// mapPodToLimiters enqueues every IOLimiter in the pod's namespace: cheap
// (namespaced list, no cluster-wide fan-out) and lets Reconcile itself
// decide relevance via the selector, keeping the mapping logic in one
// place.
func mapPodToLimiters(c client.Client) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []ctrl.Request {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			return nil
		}
		var list storagev1alpha1.IOLimiterList
		if err := c.List(ctx, &list, client.InNamespace(pod.Namespace)); err != nil {
			return nil
		}
		reqs := make([]ctrl.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return reqs
	}
}

// mapPodIOLimitToLimiters enqueues every IOLimiter listed in the
// PodIOLimit's volumes[].sources (D16's source index), so a PodIOLimit
// status update (the agent applying/failing a volume) promptly recomputes
// the IOLimiter(s) that contributed to it.
func mapPodIOLimitToLimiters() handler.MapFunc {
	return func(_ context.Context, obj client.Object) []ctrl.Request {
		pil, ok := obj.(*storagev1alpha1.PodIOLimit)
		if !ok {
			return nil
		}
		seen := map[string]struct{}{}
		var reqs []ctrl.Request
		for _, v := range pil.Spec.Volumes {
			for _, s := range v.Sources {
				if _, ok := seen[s]; ok {
					continue
				}
				seen[s] = struct{}{}
				reqs = append(reqs, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: pil.Namespace, Name: s}})
			}
		}
		return reqs
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *IOLimiterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// F9: without this, Reconcile's own Status().Update on the
		// IOLimiter it just processed re-triggers itself every time
		// (status-only changes bump resourceVersion, not generation) --
		// a self-sustaining reconcile loop on top of whatever a real spec
		// edit or a Pod/PodIOLimit event already queued.
		For(&storagev1alpha1.IOLimiter{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapPodToLimiters(mgr.GetClient())),
			builder.WithPredicates(podRelevantChangePredicate())).
		Watches(&storagev1alpha1.PodIOLimit{}, handler.EnqueueRequestsFromMapFunc(mapPodIOLimitToLimiters()),
			builder.WithPredicates(podIOLimitRelevantChangePredicate())).
		Named("iolimiter").
		Complete(r)
}
