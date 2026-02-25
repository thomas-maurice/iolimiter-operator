package limiter

import (
	"context"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"
)

// Limiter applies cgroup v2 io.max limits to containers based on pod annotations.
type Limiter struct {
	CgroupRoot string               // "/host-cgroup" in prod, t.TempDir() in tests
	ProcRoot   string               // "/host-proc" in prod, t.TempDir() in tests
	NodeName   string               // node name from Downward API
	Client     kubernetes.Interface // Interface, not *Clientset — enables fake.NewSimpleClientset()
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
	l.recoverOrphanedRules()

	for {
		if err := l.reconcile(ctx); err != nil {
			l.log.Error("reconcile error", "err", err)
		}

		select {
		case <-ctx.Done():
			l.log.Info("shutting down")
			return
		case <-time.After(ReconcileInterval):
		}
	}
}
