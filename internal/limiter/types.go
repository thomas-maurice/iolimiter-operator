package limiter

import "time"

const (
	// AnnotationLimit is the pod annotation for IO limit parameters.
	AnnotationLimit = "blkio-limiter.maurice.fr/limit"

	// AnnotationVolumePath is the pod annotation for the volume mount path.
	AnnotationVolumePath = "blkio-limiter.maurice.fr/volume-path"

	// ReconcileInterval is the time between reconciliation loops.
	ReconcileInterval = 5 * time.Second
)

// appliedRule tracks what we last wrote to a container's io.max.
type appliedRule struct {
	limit      string
	volumePath string
	cgroupPath string // kept so we can reset io.max without re-resolving the cgroup
	majMin     string // kept so we can write the reset rule
}
