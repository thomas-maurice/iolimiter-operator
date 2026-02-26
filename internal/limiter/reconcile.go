package limiter

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// reconcile lists pods on this node and applies/removes IO limits as needed.
func (l *Limiter) reconcile(ctx context.Context) error {
	pods, err := l.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + l.NodeName,
	})
	if err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	// wantLimited tracks container IDs that SHOULD have io.max rules right now.
	wantLimited := make(map[string]bool)

	for _, pod := range pods.Items {
		// Collect config.<name> and path.<name> annotations.
		configs := make(map[string]string) // name -> limit spec
		paths := make(map[string]string)   // name -> volume path

		for key, val := range pod.Annotations {
			if name, ok := strings.CutPrefix(key, AnnotationConfigPrefix); ok {
				configs[name] = val
			} else if name, ok := strings.CutPrefix(key, AnnotationPathPrefix); ok {
				paths[name] = val
			}
		}

		if len(configs) == 0 && len(paths) == 0 {
			continue
		}

		// Build the set of valid volume rules (both config and path present).
		volumes := make(map[string]volumeRule)
		for name, limitSpec := range configs {
			volumePath, hasPath := paths[name]
			if !hasPath || volumePath == "" {
				orphanedAnnotations.WithLabelValues("config").Inc()
				l.log.Warn("config annotation has no matching path annotation - skipping",
					"pod", pod.Name, "ns", pod.Namespace, "name", name)
				continue
			}
			if volumePath == "/" {
				l.log.Debug("skipping volume with path set to / - refusing to throttle the root filesystem",
					"pod", pod.Name, "ns", pod.Namespace, "name", name)
				continue
			}
			volumes[name] = volumeRule{limit: limitSpec, volumePath: volumePath}
		}

		// Warn about orphaned path annotations (path without config).
		for name := range paths {
			if _, hasConfig := configs[name]; !hasConfig {
				orphanedAnnotations.WithLabelValues("path").Inc()
				l.log.Warn("path annotation has no matching config annotation - skipping",
					"pod", pod.Name, "ns", pod.Namespace, "name", name)
			}
		}

		if len(volumes) == 0 {
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.ContainerID == "" || !cs.Ready {
				continue
			}

			containerID := stripContainerIDPrefix(cs.ContainerID)
			wantLimited[containerID] = true

			if prev, exists := l.applied[containerID]; exists {
				if volumesEqual(prev.volumes, volumes) {
					cacheHits.Inc()
					continue
				}
				l.log.Info("annotations changed, re-applying",
					"pod", pod.Name, "ns", pod.Namespace, "container", cs.Name)
			}

			log := slog.With(
				"pod", pod.Name, "ns", pod.Namespace,
				"container", cs.Name, "containerID", containerID[:12],
			)

			if result, err := l.applyIOLimits(log, containerID, volumes); err != nil {
				log.Error("failed to apply IO limits", "err", err)
				continue
			} else if result != nil {
				l.applied[containerID] = *result
			}
			// If result==nil && err==nil, no volumes had block devices yet.
			// We don't cache it so we retry on the next reconcile loop.
		}
	}

	// Reset limits for containers that are in the cache but should no longer
	// be limited.
	for id, rule := range l.applied {
		if wantLimited[id] {
			continue
		}
		l.resetIOLimits(id, rule)
		delete(l.applied, id)
	}

	return nil
}

// volumesEqual returns true if two volume maps have the same names, limits, and paths.
func volumesEqual(a, b map[string]volumeRule) bool {
	if len(a) != len(b) {
		return false
	}
	for name, va := range a {
		vb, ok := b[name]
		if !ok {
			return false
		}
		if va.limit != vb.limit || va.volumePath != vb.volumePath {
			return false
		}
	}
	return true
}
