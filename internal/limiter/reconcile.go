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

	wantLimited := make(map[string]bool)

	for _, pod := range pods.Items {
		volumes := make(map[string]volumeRule)

		for key, val := range pod.Annotations {
			name, ok := strings.CutPrefix(key, AnnotationPrefix)
			if !ok {
				continue
			}

			volumePath, limitSpec, err := parseAnnotation(val)
			if err != nil {
				l.log.Warn("invalid annotation value",
					"pod", pod.Name, "ns", pod.Namespace, "name", name, "err", err)
				continue
			}
			if volumePath == "/" {
				l.log.Debug("skipping volume with path=/ - refusing to throttle root filesystem",
					"pod", pod.Name, "ns", pod.Namespace, "name", name)
				continue
			}
			volumes[name] = volumeRule{limit: limitSpec, volumePath: volumePath}
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
		}
	}

	for id, rule := range l.applied {
		if wantLimited[id] {
			continue
		}
		l.resetIOLimits(id, rule)
		delete(l.applied, id)
	}

	return nil
}

// parseAnnotation parses an annotation value like "path=/data riops=100 wiops=50"
// into a volume path and space-separated limit spec.
func parseAnnotation(value string) (volumePath string, limitSpec string, err error) {
	parts := strings.Fields(value)
	var limits []string
	for _, p := range parts {
		if v, ok := strings.CutPrefix(p, "path="); ok {
			volumePath = v
		} else {
			limits = append(limits, p)
		}
	}
	if volumePath == "" {
		return "", "", fmt.Errorf("missing path= in annotation value")
	}
	if len(limits) == 0 {
		return "", "", fmt.Errorf("no limit parameters in annotation value")
	}
	return volumePath, strings.Join(limits, " "), nil
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
