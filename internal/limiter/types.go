package limiter

import "time"

const (
	// AnnotationConfigPrefix is the prefix for per-volume IO limit annotations.
	// Usage: blkio-limiter.maurice.fr/config.<name> = "riops=100,wiops=50"
	AnnotationConfigPrefix = "blkio-limiter.maurice.fr/config."

	// AnnotationPathPrefix is the prefix for per-volume path annotations.
	// Usage: blkio-limiter.maurice.fr/path.<name> = "/data"
	AnnotationPathPrefix = "blkio-limiter.maurice.fr/path."

	// ReconcileInterval is the time between reconciliation loops.
	ReconcileInterval = 5 * time.Second
)

// volumeRule tracks the limit applied to a single volume.
type volumeRule struct {
	limit      string
	volumePath string
	majMin     string
}

// appliedRule tracks what we last wrote to a container's io.max.
type appliedRule struct {
	volumes    map[string]volumeRule // keyed by annotation name (e.g. "data")
	cgroupPath string               // kept so we can reset io.max without re-resolving the cgroup
}
