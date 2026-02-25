package limiter

import (
	"context"
	"fmt"
	"log/slog"

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
		limit, ok := pod.Annotations[AnnotationLimit]
		if !ok {
			continue
		}

		volumePath, hasVolumePath := pod.Annotations[AnnotationVolumePath]
		if !hasVolumePath || volumePath == "" {
			l.log.Warn("pod has blkio-limiter.maurice.fr/limit but no blkio-limiter.maurice.fr/volume-path — skipping (volume-path is required to avoid throttling the wrong device)",
				"pod", pod.Name, "ns", pod.Namespace)
			continue
		}
		if volumePath == "/" {
			l.log.Debug("skipping pod with volume-path set to / — refusing to throttle the root filesystem (it may be overlay or the node's root disk, neither of which should be limited)",
				"pod", pod.Name, "ns", pod.Namespace)
			continue
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if cs.ContainerID == "" || !cs.Ready {
				continue
			}

			containerID := stripContainerIDPrefix(cs.ContainerID)
			wantLimited[containerID] = true

			if prev, exists := l.applied[containerID]; exists {
				if prev.limit == limit && prev.volumePath == volumePath {
					continue
				}
				l.log.Info("annotation changed, re-applying",
					"pod", pod.Name, "ns", pod.Namespace, "container", cs.Name,
					"old_limit", prev.limit, "new_limit", limit)
			}

			log := slog.With(
				"pod", pod.Name, "ns", pod.Namespace,
				"container", cs.Name, "containerID", containerID[:12],
				"limit", limit, "volumePath", volumePath,
			)

			if result, err := l.applyIOLimit(log, containerID, limit, volumePath); err != nil {
				log.Error("failed to apply IO limit", "err", err)
				continue
			} else if result != nil {
				l.applied[containerID] = *result
			}
			// If result==nil && err==nil, the volume has no block device yet.
			// We don't cache it so we retry on the next reconcile loop.
		}
	}

	// Reset limits for containers that are in the cache but should no longer
	// be limited.
	for id, rule := range l.applied {
		if wantLimited[id] {
			continue
		}
		l.resetIOLimit(id, rule)
		delete(l.applied, id)
	}

	return nil
}
