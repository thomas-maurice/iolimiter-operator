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

// Package agent implements SPEC.md §6.1: the node agent that watches
// PodIOLimits scheduled on its node and enforces them via cgroup v2 io.max
// on the pod-level cgroup, with no PIDs and no tree walk (D6). It never
// takes a path from the spec: every path is built from the pod's UID/QoS
// (internal/cgroup) and the host's own mountinfo (internal/mountinfo,
// internal/blockdev).
package agent

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/thomas-maurice/iolimiter-operator/api/v1alpha1"
	"github.com/thomas-maurice/iolimiter-operator/internal/cgroup"
	"github.com/thomas-maurice/iolimiter-operator/internal/iomax"
	"github.com/thomas-maurice/iolimiter-operator/internal/mountinfo"
)

// pendingRequeueInterval paces the wait while any volume is Pending (§6.1
// step 10: cgroup/mount not there yet).
const pendingRequeueInterval = 2 * time.Second

// requeueSoon retries a reconcile that hit an optimistic-lock conflict on
// the status write: the object's own change already triggers a
// watch-driven requeue, this is a bound in case that races.
const requeueSoon = 100 * time.Millisecond

// noLine is iomax's sentinel for "no io.max line for this device".
const noLine = "<none>"

// ioMaxWrite is iomax.Write as a package-var seam (same pattern as
// iomax's own openForWrite): tests substitute it to force KernelRejected
// (return an error) or KernelMismatch (return a WriteResult whose After
// doesn't match the rule that was asked for) without a real kernel.
var ioMaxWrite = iomax.Write

// PodIOLimitReconciler implements SPEC.md §6.1: one PodIOLimit on this
// node's io.max enforcement.
type PodIOLimitReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Pod events (D26): the agent never reads the Pod, so
	// the event reference is built from spec.podName/podUID alone.
	Recorder events.EventRecorder

	NodeName       string
	Layout         cgroup.Layout
	CgroupRoot     string // e.g. /host/cgroup
	ProcRoot       string // e.g. /host/proc
	KubeletRootDir string // e.g. /var/lib/kubelet
	SysRoot        string // e.g. /sys (the container's own sysfs, D14)
	ResyncPeriod   time.Duration
	// ProtectedPaths is D34's refused-device set (replaces D23's root +
	// kubelet-dir pair): the whole-disk device backing any of these host
	// paths is refused (Unsupported/SharedRootDevice) unless
	// AllowRootDevice.
	ProtectedPaths  []string
	AllowRootDevice bool

	Health *health

	// cacheHasObjectsMu guards lastCacheHasObjects: cacheHasObjects keeps
	// the previous known value on a transient List error instead of
	// flipping D24 liveness state to "no objects" on every hiccup.
	cacheHasObjectsMu   sync.Mutex
	lastCacheHasObjects bool
}

// +kubebuilder:rbac:groups=storage.maurice.fr,resources=podiolimits,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.maurice.fr,resources=podiolimits/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile implements SPEC.md §6.1.
func (r *PodIOLimitReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// F5: recorded unconditionally, including the NotFound path below (the
	// last PodIOLimit on this node being deleted must not freeze
	// hasObjects/lastReconcileDone and trip D24(b) ~3 resyncs later). The
	// closure re-reads the cache at defer-time (reconcile completion), not
	// at entry, so hasObjects reflects this reconcile's own outcome -- this
	// also keeps iolimiter_agent_owned_devices fresh on every reconcile,
	// including release/NotFound (F8: it must not go stale once the last
	// object is gone), since cacheHasObjects sets that gauge as a
	// side effect of the same cache List.
	if r.Health != nil {
		defer func() {
			r.Health.recordReconcile(r.cacheHasObjects(ctx))
		}()
	}

	var pil storagev1alpha1.PodIOLimit
	if err := r.Get(ctx, req.NamespacedName, &pil); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log := logf.FromContext(ctx).WithValues("pod", pil.Namespace+"/"+pil.Spec.PodName, "podiolimit", pil.Namespace+"/"+pil.Name)
	ctx = logf.IntoContext(ctx, log)

	// Step 1: defense in depth. The field-selected watch (D16) should never
	// hand us another node's object.
	if pil.Spec.NodeName != r.NodeName {
		log.V(1).Info("Ignoring PodIOLimit for another node", "node", pil.Spec.NodeName)
		return ctrl.Result{}, nil
	}

	if pil.DeletionTimestamp != nil {
		return r.release(ctx, &pil)
	}

	// D39: defense in depth. The CRD pattern on podUID should already
	// reject a non-UUID value at admission; this catches an object that
	// predates the pattern (upgrade) or was otherwise written around it,
	// before it ever reaches cgroup path math.
	if _, err := uuid.Parse(string(pil.Spec.PodUID)); err != nil {
		log.Error(err, "podUID is not a valid UUID, refusing to compute a cgroup path", "podUID", pil.Spec.PodUID)
		return r.writeUniformStatus(ctx, &pil, "", stateFailed, "InvalidSpec",
			fmt.Sprintf("podUID %q is not a valid UUID", pil.Spec.PodUID), r.jitteredResync())
	}

	podCg, err := r.Layout.PodPath(pil.Spec.PodUID, pil.Spec.QOSClass)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("computing pod cgroup path: %w", err)
	}
	cgroupPath := relCgroupPath(r.CgroupRoot, podCg)

	if _, statErr := os.Stat(podCg); statErr != nil {
		if !os.IsNotExist(statErr) {
			return ctrl.Result{}, fmt.Errorf("stat pod cgroup %s: %w", podCg, statErr)
		}
		return r.writeUniformStatus(ctx, &pil, cgroupPath, statePending, "CgroupNotFound", "pod cgroup not found yet", pendingRequeueInterval)
	}

	enabled, err := cgroup.IOControllerEnabled(podCg)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking io controller on %s: %w", podCg, err)
	}
	if !enabled {
		return r.writeUniformStatus(ctx, &pil, cgroupPath, stateFailed, "IOControllerDisabled", "io controller not enabled on pod cgroup", r.jitteredResync())
	}

	entries, err := readMountinfo(r.ProcRoot)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading mountinfo: %w", err)
	}

	prevVolumes := volumeStatusByName(&pil)

	// pendingEvents buffers every D26 Pod event this reconcile decides to
	// emit; none of them is actually sent until the final status write
	// below succeeds (D26: "emitted after the status write succeeds"). A
	// conflicted write means this reconcile's outcome never became visible
	// server-side, so its events must never fire either. The requeued
	// retry must still emit the event exactly once: it can't rely on live
	// kernel state alone to decide "did a transition happen" (a retry
	// after a conflict sees the kernel already matching, since the write
	// that conflicted on status had already landed on the kernel) -- it
	// derives that from persisted status (ownedBefore.State/Rule) vs the
	// desired rule instead, in applyDevice.
	var pendingEvents []pendingEvent

	resolved := make([]resolution, 0, len(pil.Spec.Volumes))
	volumeOrder := make([]string, 0, len(pil.Spec.Volumes))
	for _, v := range pil.Spec.Volumes {
		res := resolveVolume(entries, r.SysRoot, r.KubeletRootDir, pil.Spec.PodUID, r.ProtectedPaths, r.AllowRootDevice, v)
		resolved = append(resolved, res)
		volumeOrder = append(volumeOrder, v.Name)
	}
	resByName := make(map[string]resolution, len(resolved))
	for _, res := range resolved {
		resByName[res.Volume.Name] = res
	}

	groups := groupByDevice(resolved)
	desired := make(map[string]iomax.Rule, len(groups))
	for dev, g := range groups {
		desired[dev] = g.Rule
	}
	owned := ownedByDevice(&pil)

	outcomes := map[string]volumeOutcome{}
	for _, res := range resolved {
		r.logResolution(log, res, prevVolumes[res.Volume.Name])
		if res.State == "" {
			continue // resolved to a device; outcome decided by the apply loop below.
		}
		outcomes[res.Volume.Name] = volumeOutcome{
			device:      res.Device,
			mountDevice: mountDeviceField(res),
			state:       res.State,
			reason:      res.Reason,
			message:     res.Message,
		}
		// F17: emitVolumeTransitionEvent had exactly one case
		// (Unsupported, on transition) once events stopped firing
		// unconditionally; inlined rather than kept as a single-branch
		// method.
		if prev := prevVolumes[res.Volume.Name]; res.State == stateUnsupported && (prev.State != res.State || prev.Reason != res.Reason) {
			pendingEvents = append(pendingEvents, pendingEvent{
				eventtype: corev1.EventTypeWarning,
				reason:    "IOLimitSkipped",
				message:   fmt.Sprintf("volume %s: %s: %s", res.Volume.Name, res.Reason, res.Message),
			})
		}
	}

	// Step 6/7: record intent for any new-or-changed device BEFORE any
	// kernel write (D8). Devices already owned with an identical rule are
	// left untouched here; drift on those is handled by the resync compare
	// in the apply loop below. owned is intentionally left as it was BEFORE
	// this call: applyDevice below needs each device's ownership as it was
	// before the intent write (to tell "brand new" from "already owned,
	// rule changed") for the Applied-vs-Updated event choice, not the
	// Pending placeholder recordDeviceIntent may just have written.
	conflict, err := r.recordDeviceIntent(ctx, &pil, owned, desired)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflict {
		return ctrl.Result{RequeueAfter: requeueSoon}, nil
	}

	// Step 8: kernel writes, plus drift detection on already-owned devices.
	ioMaxPath := filepath.Join(podCg, "io.max")
	content, readErr := os.ReadFile(ioMaxPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return ctrl.Result{}, fmt.Errorf("reading %s: %w", ioMaxPath, readErr)
	}
	current := iomax.Parse(string(content))

	finalDevices := ownedByDevice(&pil)
	for dev, g := range groups {
		r.applyDevice(log, ioMaxPath, dev, g, current, owned[dev], resByName, prevVolumes, finalDevices, outcomes, &pendingEvents)
	}

	// Step 9: reset every stale (no-longer-desired) owned device.
	for dev, od := range owned {
		if _, stillDesired := desired[dev]; stillDesired {
			continue
		}
		r.resetDevice(log, ioMaxPath, dev, od, finalDevices, &pendingEvents)
	}

	pil.Status.Devices = buildDeviceList(finalDevices)
	pil.Status.Volumes = buildVolumeStatuses(volumeOrder, outcomes)
	pil.Status.CgroupPath = cgroupPath
	pil.Status.ObservedGeneration = pil.Generation
	status, reason := readyReason(pil.Status.Volumes)
	message := readyMessage(pil.Status.Volumes)
	setReadyCondition(&pil, status, reason, message)

	if err := r.writeStatusIfChanged(ctx, &pil); err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("Status update conflict, requeueing")
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, err
	}
	r.flushEvents(&pil, pendingEvents)

	if reason == statePending {
		return r.pendingRequeue(ctx, &pil), nil
	}
	return ctrl.Result{RequeueAfter: r.jitteredResync()}, nil
}

// pendingEvent is a D26 Pod event decided during a reconcile but not yet
// sent: it is buffered until the reconcile's status write actually
// succeeds (D26: "emitted after the status write succeeds"), so a
// conflicted write never emits an event for a transition the server never
// recorded.
type pendingEvent struct {
	eventtype, reason, message string
}

// flushEvents sends every buffered event, called only once the status
// write it was computed alongside has succeeded.
func (r *PodIOLimitReconciler) flushEvents(pil *storagev1alpha1.PodIOLimit, evs []pendingEvent) {
	for _, ev := range evs {
		r.emitPodEvent(pil, ev.eventtype, ev.reason, ev.message)
	}
}

// recordDeviceIntent implements SPEC.md §6.1 step 6/7: for every device in
// desired that isn't owned yet, or whose rule differs from what's owned,
// Status().Update pil with that device recorded Pending BEFORE any kernel
// write happens (D8). Devices already owned with an identical rule are
// left alone. On an optimistic-lock conflict it returns conflict=true and
// a nil error: the caller must requeue without touching the kernel. On
// success (including "nothing to do"), *pil reflects the latest object.
func (r *PodIOLimitReconciler) recordDeviceIntent(ctx context.Context, pil *storagev1alpha1.PodIOLimit, owned map[string]storagev1alpha1.OwnedDevice, desired map[string]iomax.Rule) (conflict bool, err error) {
	log := logf.FromContext(ctx)
	var changedDevices []string
	for dev, rule := range desired {
		line := rule.Format(dev)
		if od, ok := owned[dev]; !ok || od.Rule != line {
			changedDevices = append(changedDevices, dev)
		}
	}
	if len(changedDevices) == 0 {
		return false, nil
	}

	updated := pil.DeepCopy()
	newDevices := ownedByDevice(pil)
	for _, dev := range changedDevices {
		line := desired[dev].Format(dev)
		log.Info("Recording device intent before kernel write", "device", dev, "rule", line, "state", statePending)
		newDevices[dev] = storagev1alpha1.OwnedDevice{Device: dev, Rule: line, State: statePending}
	}
	updated.Status.Devices = buildDeviceList(newDevices)
	if err := r.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			log.V(1).Info("Status conflict recording device intent, requeueing; kernel untouched")
			return true, nil
		}
		return false, fmt.Errorf("recording device intent: %w", err)
	}
	*pil = *updated
	return false, nil
}

// applyDevice implements the per-device half of §6.1 step 8: skip a
// verified no-drift device, otherwise write and verify, updating outcomes
// for every volume sharing dev and emitting the D26 event/log for the
// transition (apply/update/drift-correct/fail).
func (r *PodIOLimitReconciler) applyDevice(log logr.Logger, ioMaxPath, dev string, g *deviceGroup, current map[string]iomax.Rule, ownedBefore storagev1alpha1.OwnedDevice, resByName map[string]resolution, prevVolumes map[string]storagev1alpha1.VolumeStatus, finalDevices map[string]storagev1alpha1.OwnedDevice, outcomes map[string]volumeOutcome, evs *[]pendingEvent) {
	line := g.Rule.Format(dev)
	curLine := lineFor(current, dev)
	wasApplied := ownedBefore.State == stateApplied && ownedBefore.Rule == line

	setOutcomes := func(state, reason, message string) {
		for _, name := range g.Volumes {
			res := resByName[name]
			outcomes[name] = volumeOutcome{
				device:      dev,
				mountDevice: mountDeviceField(res),
				state:       state,
				reason:      reason,
				message:     message,
			}
		}
	}
	// setAppliedOutcomes gives each volume in the group its own message
	// naming the OTHER volumes it shares dev with (D7), not itself.
	setAppliedOutcomes := func() {
		reason := sharedDeviceReason(g)
		for _, name := range g.Volumes {
			res := resByName[name]
			outcomes[name] = volumeOutcome{
				device:      dev,
				mountDevice: mountDeviceField(res),
				state:       stateApplied,
				reason:      reason,
				message:     sharedDeviceMessage(g, name, dev, line),
			}
		}
	}

	// volumeRuleMessage renders D26's "volume→device rule" event message
	// shape, naming every volume sharing dev. Defined before the curLine==
	// line branch below so both it and the successful-kernel-write path
	// further down can share it.
	volumeRuleMessage := func() string {
		return fmt.Sprintf("%s→%s: %s", strings.Join(g.Volumes, ","), dev, line)
	}

	if curLine == line {
		finalDevices[dev] = storagev1alpha1.OwnedDevice{Device: dev, Rule: line, State: stateApplied}
		setAppliedOutcomes()
		if wasApplied {
			// Status already recorded this exact rule as Applied: a genuine
			// no-op resync, nothing changed.
			log.V(1).Info("Resync: no drift on device", "device", dev)
			return
		}
		// The kernel already satisfies the desired rule, but persisted
		// status did not yet record it as Applied with this exact rule --
		// either this is a brand-new device/changed rule this reconcile
		// never got to write (recordDeviceIntent only writes Pending, the
		// kernel write below normally follows), or -- the case this
		// branch exists to fix -- a PRIOR reconcile already wrote this
		// exact rule to the kernel and computed this same transition, but
		// its final status write conflicted (D8/D26: events only fire
		// after a successful status write) and the event was buffered but
		// never sent. Either way, treat it exactly like the
		// successful-kernel-write path below: the transition is
		// completing (or being recorded as complete) now, so the same
		// event/log choice applies, without ever touching the kernel
		// again since it's already correct.
		log.Info("Kernel already satisfies desired rule", "device", dev, "rule", line)
		if ownedBefore.Device != "" {
			*evs = append(*evs, pendingEvent{corev1.EventTypeNormal, "IOLimitUpdated", volumeRuleMessage()})
		} else {
			*evs = append(*evs, pendingEvent{corev1.EventTypeNormal, "IOLimitApplied", volumeRuleMessage()})
		}
		return
	}

	// failureIsRepeat reports whether every volume in g already recorded
	// this exact (Failed, reason) before this reconcile: if so, this is a
	// retried write that failed the same way again, not a new transition,
	// and IOLimitFailed must not fire again (D26: events only on
	// transition, never per resync).
	failureIsRepeat := func(reason string) bool {
		for _, name := range g.Volumes {
			prev := prevVolumes[name]
			if prev.State != stateFailed || prev.Reason != reason {
				return false
			}
		}
		return true
	}

	res, werr := ioMaxWrite(ioMaxPath, dev, g.Rule)
	recordKernelWrite("apply", werr)
	if werr != nil {
		log.Info("Kernel write failed", "op", "apply", "path", ioMaxPath, "device", dev, "before", res.Before, "rule", line, "error", werr.Error())
		setOutcomes(stateFailed, "KernelRejected", werr.Error())
		if !failureIsRepeat("KernelRejected") {
			*evs = append(*evs, pendingEvent{corev1.EventTypeWarning, "IOLimitFailed",
				fmt.Sprintf("%s→%s: KernelRejected: %s", strings.Join(g.Volumes, ","), dev, werr.Error())})
		}
		return
	}

	log.Info("Kernel write", "op", "apply", "path", ioMaxPath, "device", dev, "before", res.Before, "rule", line, "after", res.After)

	if res.After != line {
		msg := fmt.Sprintf("wrote %q, read back %q", line, res.After)
		setOutcomes(stateFailed, "KernelMismatch", msg)
		if !failureIsRepeat("KernelMismatch") {
			*evs = append(*evs, pendingEvent{corev1.EventTypeWarning, "IOLimitFailed",
				fmt.Sprintf("%s→%s: KernelMismatch: %s", strings.Join(g.Volumes, ","), dev, msg)})
		}
		return
	}

	finalDevices[dev] = storagev1alpha1.OwnedDevice{Device: dev, Rule: line, State: stateApplied}
	setAppliedOutcomes()

	switch {
	case wasApplied:
		// Rule we already believed was Applied, but the kernel's line had
		// drifted (an external write, or the cgroup being re-realized).
		recordDriftCorrection()
		log.Info("Drift corrected", "op", "apply", "device", dev, "before", res.Before, "rule", line)
		*evs = append(*evs, pendingEvent{corev1.EventTypeWarning, "IOLimitDriftCorrected", fmt.Sprintf("device %s: rule reapplied, was %q", dev, res.Before)})
	case ownedBefore.Device != "":
		*evs = append(*evs, pendingEvent{corev1.EventTypeNormal, "IOLimitUpdated", volumeRuleMessage()})
	default:
		*evs = append(*evs, pendingEvent{corev1.EventTypeNormal, "IOLimitApplied", volumeRuleMessage()})
	}
}

func (r *PodIOLimitReconciler) resetDevice(log logr.Logger, ioMaxPath, dev string, od storagev1alpha1.OwnedDevice, finalDevices map[string]storagev1alpha1.OwnedDevice, evs *[]pendingEvent) {
	res, err := iomax.Reset(ioMaxPath, dev)
	recordKernelWrite("reset", err)
	if err != nil {
		// Could not reset: keep ownership so a future reconcile retries,
		// rather than silently losing track of a rule that may still be
		// live in the kernel (D8).
		log.Info("Kernel reset failed, keeping ownership", "op", "reset", "path", ioMaxPath, "device", dev, "error", err.Error())
		finalDevices[dev] = od
		return
	}
	log.Info("Kernel reset", "op", "reset", "path", ioMaxPath, "device", dev, "before", res.Before, "after", res.After)
	delete(finalDevices, dev)
	*evs = append(*evs, pendingEvent{corev1.EventTypeNormal, "IOLimitReset", fmt.Sprintf("device %s reset: volume removed", dev)})
}

// release implements §6.1 step 11: reset every owned device (a missing
// cgroup counts as reset, via iomax.Reset's ENOENT handling), then clear
// ownership and mark Released. Never writes a non-reset rule while the
// object is being deleted.
func (r *PodIOLimitReconciler) release(ctx context.Context, pil *storagev1alpha1.PodIOLimit) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if len(pil.Status.Devices) == 0 {
		// D9: still write Ready=False/Released even with nothing to reset,
		// so a PodIOLimit that never owned anything (e.g. deleted before
		// its first successful apply) gets the same terminal status as one
		// that did -- consistent for anything reading status, and
		// idempotent (writeStatusIfChanged no-ops once it's already set).
		log.V(1).Info("Release: nothing owned")
		updated := pil.DeepCopy()
		setReadyCondition(updated, metav1.ConditionFalse, "Released", "no devices were ever owned")
		if err := r.writeStatusIfChanged(ctx, updated); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: requeueSoon}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	podCg, err := r.Layout.PodPath(pil.Spec.PodUID, pil.Spec.QOSClass)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("computing pod cgroup path: %w", err)
	}
	ioMaxPath := filepath.Join(podCg, "io.max")

	log.Info("Release starting", "devices", len(pil.Status.Devices))

	remaining := ownedByDevice(pil)
	for _, od := range pil.Status.Devices {
		res, err := iomax.Reset(ioMaxPath, od.Device)
		recordKernelWrite("reset", err)
		if err != nil {
			log.Info("Kernel reset failed during release, keeping ownership", "op", "reset", "path", ioMaxPath, "device", od.Device, "error", err.Error())
			continue
		}
		log.Info("Kernel reset", "op", "reset", "path", ioMaxPath, "device", od.Device, "before", res.Before, "after", res.After)
		delete(remaining, od.Device)
	}

	updated := pil.DeepCopy()
	updated.Status.Devices = buildDeviceList(remaining)
	if len(updated.Status.Devices) > 0 {
		// Some devices could not be reset: stay Terminating, retry.
		if err := r.writeStatusIfChanged(ctx, updated); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: requeueSoon}, nil
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pendingRequeueInterval}, nil
	}

	setReadyCondition(updated, metav1.ConditionFalse, "Released", "all owned devices reset")
	if err := r.writeStatusIfChanged(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, err
	}
	log.Info("Release finished", "op", "release")
	r.emitPodEvent(pil, corev1.EventTypeNormal, "IOLimitReset", "all rules released")
	return ctrl.Result{}, nil
}

func (r *PodIOLimitReconciler) writeUniformStatus(ctx context.Context, pil *storagev1alpha1.PodIOLimit, cgroupPath, volState, reason, message string, requeue time.Duration) (ctrl.Result, error) {
	updated := pil.DeepCopy()
	outcomes := map[string]volumeOutcome{}
	order := make([]string, 0, len(pil.Spec.Volumes))
	for _, v := range pil.Spec.Volumes {
		order = append(order, v.Name)
		outcomes[v.Name] = volumeOutcome{state: volState, reason: reason, message: message}
	}
	updated.Status.Volumes = buildVolumeStatuses(order, outcomes)
	updated.Status.CgroupPath = cgroupPath
	updated.Status.ObservedGeneration = updated.Generation

	status, condReason := readyReason(updated.Status.Volumes)
	setReadyCondition(updated, status, condReason, message)

	if err := r.writeStatusIfChanged(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: requeueSoon}, nil
		}
		return ctrl.Result{}, err
	}
	if volState == statePending {
		// F4: back off rather than the caller's fixed interval (see
		// pendingRequeue).
		return r.pendingRequeue(ctx, updated), nil
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// writeStatusIfChanged writes pil's status only if it semantically changed
// from what the client currently holds server-side (D11), logging the
// Ready transition and never at Info on a no-op.
func (r *PodIOLimitReconciler) writeStatusIfChanged(ctx context.Context, pil *storagev1alpha1.PodIOLimit) error {
	log := logf.FromContext(ctx)
	var before storagev1alpha1.PodIOLimit
	if err := r.Get(ctx, client.ObjectKeyFromObject(pil), &before); err != nil {
		return fmt.Errorf("re-reading PodIOLimit before status write: %w", err)
	}
	// F6: before is read through the cache and can lag behind a
	// resourceVersion pil already carries from this same reconcile's own
	// live recordDeviceIntent Status().Update (D8 step 7) -- overwriting
	// pil.ResourceVersion with before's would submit a stale RV and make
	// nearly every first apply 409. pil's own ResourceVersion (whatever it
	// is: the fresh one from that live write, or the cached one from the
	// Get at the top of Reconcile if no intent write happened this pass)
	// is never staler than before's, so it is left untouched; before is
	// used only for the semantic-equality comparison below.
	if statusSemanticallyEqual(before.Status, pil.Status) {
		log.V(1).Info("Status unchanged, not written")
		return nil
	}

	oldReady := findReady(before.Status.Conditions)
	newReady := findReady(pil.Status.Conditions)

	if err := r.Status().Update(ctx, pil); err != nil {
		return err
	}

	if oldReady == nil || oldReady.Status != newReady.Status || oldReady.Reason != newReady.Reason {
		log.Info("PodIOLimit Ready transition", "from", conditionReasonOf(oldReady), "to", newReady.Reason,
			"observedGeneration", pil.Status.ObservedGeneration)
	}
	return nil
}

func findReady(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == "Ready" {
			return &conds[i]
		}
	}
	return nil
}

func conditionReasonOf(c *metav1.Condition) string {
	if c == nil {
		return "<none>"
	}
	return c.Reason
}

// logResolution logs every volume's resolution at Info -- "one resolution
// line per volume" (§6.3), for both a successful whole-disk resolution and
// a skip (Pending/Unsupported) -- but only on transition (device/mount/
// state/reason unchanged from prev logs nothing at Info, per D26's "no-op
// resync" rule). It always increments the resolution metric: steady-state
// counting doesn't need to be log-gated.
func (r *PodIOLimitReconciler) logResolution(log logr.Logger, res resolution, prev storagev1alpha1.VolumeStatus) {
	metricResult := res.Reason
	if metricResult == "" {
		metricResult = stateApplied
	}
	recordVolumeResolution(metricResult)

	effState := res.State
	if effState == "" {
		effState = stateApplied // resolution succeeded; final Applied/Failed is decided by the apply loop, but status already reads Applied once it has.
	}
	unchanged := prev.State == effState && prev.Device == res.Device && prev.MountDevice == mountDeviceField(res)
	if res.State != "" {
		// Reason is only meaningful here for a Pending/Unsupported
		// resolution (resolveVolume's own doc comment: Applied/Failed are
		// decided later). A resolved volume's persisted Reason (e.g. D7's
		// "SharedDevice") is set by the apply loop, not by resolution --
		// comparing it against res.Reason (always "" for a resolved
		// volume) would never match and log Info every single reconcile
		// for any volume sharing a device with another.
		unchanged = unchanged && prev.Reason == res.Reason
	}
	if unchanged {
		log.V(1).Info("Resync: unchanged volume resolution", "volume", res.Volume.Name, "state", res.State, "reason", res.Reason)
		return
	}
	log.Info("Volume resolution", "volume", res.Volume.Name, "kubeletDir", res.Volume.KubeletDirName,
		"mountPoint", res.MountPoint, "mountDevice", res.MountDevice, "partitionOf", res.PartitionOf,
		"device", res.Device, "state", res.State, "reason", res.Reason, "message", res.Message)
}

// emitPodEvent records an event on the Pod referenced by
// spec.podName/spec.podUID, without ever reading the Pod (D16/D26): the
// agent has no RBAC to get/list/watch Pods.
func (r *PodIOLimitReconciler) emitPodEvent(pil *storagev1alpha1.PodIOLimit, eventtype, reason, message string) {
	if r.Recorder == nil {
		return
	}
	ref := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pil.Spec.PodName,
			Namespace: pil.Namespace,
			UID:       pil.Spec.PodUID,
		},
	}
	r.Recorder.Eventf(ref, nil, eventtype, reason, reason, "%s", message)
}

// jitteredResync returns ResyncPeriod +/- up to 10%, so many agents on
// many nodes don't all resync in lockstep.
func (r *PodIOLimitReconciler) jitteredResync() time.Duration {
	base := r.ResyncPeriod
	if base <= 0 {
		base = time.Minute
	}
	jitter := base / 10
	if jitter <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(jitter)))
}

// pendingRequeue implements F4's backed-off Pending poll and F8's elapsed
// log key: elapsed is time since the Ready condition first turned Pending
// (Ready.LastTransitionTime, already updated by the writeStatusIfChanged
// call this always follows), so no separate state needs tracking.
func (r *PodIOLimitReconciler) pendingRequeue(ctx context.Context, pil *storagev1alpha1.PodIOLimit) ctrl.Result {
	var elapsed time.Duration
	if c := findReady(pil.Status.Conditions); c != nil && !c.LastTransitionTime.IsZero() {
		elapsed = time.Since(c.LastTransitionTime.Time)
	}
	requeue := pendingBackoff(elapsed, r.ResyncPeriod)
	logf.FromContext(ctx).V(1).Info("Requeueing while Pending", "elapsed", elapsed.Round(time.Second).String(), "requeueAfter", requeue)
	return ctrl.Result{RequeueAfter: requeue}
}

// pendingBackoff implements F4: a volume that never mounts must not poll
// every pendingRequeueInterval forever. The first few retries stay fast
// (doubling from pendingRequeueInterval), growing up to max (the agent's
// --resync-period, which already re-checks everything anyway) once elapsed
// pending time shows the wait is not short-lived. Pure function of
// elapsed/base/max: no counter needs to be threaded through status.
func pendingBackoff(elapsed, max time.Duration) time.Duration {
	const base = pendingRequeueInterval
	if max <= 0 || max < base {
		max = base
	}
	interval := base
	for elapsed >= interval*2 && interval < max {
		interval *= 2
	}
	if interval > max {
		interval = max
	}
	return interval
}

// cacheHasObjects reports whether the agent's cache currently holds at
// least one PodIOLimit (D24's liveness precondition (b)). A transient List
// error must not itself flip liveness state: it's logged at V(1) and the
// previously known value is kept, rather than treating "the cache failed to
// answer" the same as "the cache answered zero".
func (r *PodIOLimitReconciler) cacheHasObjects(ctx context.Context) bool {
	var list storagev1alpha1.PodIOLimitList
	if err := r.List(ctx, &list); err != nil {
		logf.FromContext(ctx).V(1).Info("Listing PodIOLimits for liveness check failed, keeping previous value", "error", err.Error())
		r.cacheHasObjectsMu.Lock()
		defer r.cacheHasObjectsMu.Unlock()
		return r.lastCacheHasObjects
	}
	setOwnedDevicesGauge(sumOwnedDevices(list.Items))
	hasObjects := len(list.Items) > 0
	r.cacheHasObjectsMu.Lock()
	r.lastCacheHasObjects = hasObjects
	r.cacheHasObjectsMu.Unlock()
	return hasObjects
}

func sumOwnedDevices(items []storagev1alpha1.PodIOLimit) int {
	n := 0
	for _, pil := range items {
		n += len(pil.Status.Devices)
	}
	return n
}

func lineFor(current map[string]iomax.Rule, dev string) string {
	rule, ok := current[dev]
	if !ok {
		return noLine
	}
	return rule.Format(dev)
}

func mountDeviceField(res resolution) string {
	if res.PartitionOf == "" {
		return ""
	}
	return res.MountDevice
}

func sharedDeviceReason(g *deviceGroup) string {
	if len(g.Volumes) > 1 {
		return "SharedDevice"
	}
	return ""
}

func sharedDeviceMessage(g *deviceGroup, self, dev, line string) string {
	if len(g.Volumes) <= 1 {
		return ""
	}
	return fmt.Sprintf("shares device %s with %s: %s", dev, strings.Join(otherVolumes(g.Volumes, self), ", "), line)
}

func readyMessage(volumes []storagev1alpha1.VolumeStatus) string {
	var problems []string
	for _, v := range volumes {
		if v.State == stateFailed || v.State == statePending {
			problems = append(problems, fmt.Sprintf("%s: %s: %s", v.Name, v.Reason, v.Message))
		}
	}
	return strings.Join(problems, "; ")
}

func relCgroupPath(root, podCg string) string {
	rel, err := filepath.Rel(root, podCg)
	if err != nil {
		return podCg
	}
	return rel
}

// readMountinfo parses <procRoot>/1/mountinfo (D14: the only file read from
// the read-only /host/proc mount).
func readMountinfo(procRoot string) ([]mountinfo.Entry, error) {
	f, err := os.Open(filepath.Join(procRoot, "1", "mountinfo"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return mountinfo.Parse(f)
}

// SetupWithManager sets up the controller with the Manager.
func (r *PodIOLimitReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.PodIOLimit{}).
		Named("podiolimit").
		Complete(r)
}
