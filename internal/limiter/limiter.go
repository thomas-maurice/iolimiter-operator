package limiter

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/kubernetes"
)

const (
	// AnnotationPrefix is the prefix for blkio-limiter annotations.
	// Usage: blkio-limiter.maurice.fr/<name> = "path=/data riops=100 wiops=50"
	AnnotationPrefix = "blkio-limiter.maurice.fr/"

	// ReconcileInterval is the time between reconciliation loops.
	ReconcileInterval = 5 * time.Second
)

// volumeRule tracks the limit applied to a single volume.
type volumeRule struct {
	limit      string // space-separated limit params, e.g. "riops=100 wiops=50"
	volumePath string
	majMin     string // resolved at apply time, e.g. "7:0"
}

// appliedRule tracks what we last wrote to a container's io.max.
type appliedRule struct {
	volumes    map[string]volumeRule // keyed by annotation name (e.g. "data")
	cgroupPath string               // kept so we can reset without re-resolving
}

// Limiter applies cgroup v2 io.max limits to containers based on pod annotations.
type Limiter struct {
	CgroupRoot string
	ProcRoot   string
	NodeName   string
	Client     kubernetes.Interface
	applied    map[string]appliedRule
	log        *slog.Logger
}

// New creates a Limiter with the given configuration.
func New(cgroupRoot, procRoot, nodeName string, client kubernetes.Interface, logger *slog.Logger) *Limiter {
	return &Limiter{
		CgroupRoot: cgroupRoot,
		ProcRoot:   procRoot,
		NodeName:   nodeName,
		Client:     client,
		applied:    make(map[string]appliedRule),
		log:        logger,
	}
}

// Run starts the reconciliation loop. It blocks forever.
func (l *Limiter) Run(ctx context.Context) {
	l.resetAllRules()

	for {
		timer := prometheus.NewTimer(reconcileDuration)
		err := l.reconcile(ctx)
		timer.ObserveDuration()

		if err != nil {
			reconcileErrors.Inc()
			l.log.Error("reconcile error", "err", err)
		}

		limitedContainers.Set(float64(len(l.applied)))

		select {
		case <-ctx.Done():
			l.log.Info("shutting down")
			return
		case <-time.After(ReconcileInterval):
		}
	}
}
