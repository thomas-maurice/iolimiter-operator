package limiter

import "github.com/prometheus/client_golang/prometheus"

var (
	reconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "blkio_limiter_reconcile_duration_seconds",
		Help:    "Time spent per reconcile loop.",
		Buckets: prometheus.DefBuckets,
	})

	reconcileErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "blkio_limiter_reconcile_errors_total",
		Help: "Failed reconcile loops.",
	})

	limitedContainers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "blkio_limiter_limited_containers",
		Help: "Containers currently with active io.max rules.",
	})

	limitedVolumes = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "blkio_limiter_limited_volumes",
		Help: "Individual volume rules currently applied.",
	})

	applyTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "blkio_limiter_apply_total",
		Help: "io.max write operations.",
	}, []string{"status"})

	resetTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "blkio_limiter_reset_total",
		Help: "Limit resets (annotation removed or pod gone).",
	})

	cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "blkio_limiter_cache_hits_total",
		Help: "Reconcile skips (nothing changed).",
	})

	orphanedAnnotations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "blkio_limiter_orphaned_annotations_total",
		Help: "Unpaired annotations.",
	}, []string{"kind"})

	deviceResolutionFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "blkio_limiter_device_resolution_failures_total",
		Help: "Volumes with no block device.",
	})

	recoveredRules = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "blkio_limiter_recovered_rules_total",
		Help: "Orphaned io.max rules found on startup.",
	})
)

func init() {
	prometheus.MustRegister(
		reconcileDuration,
		reconcileErrors,
		limitedContainers,
		limitedVolumes,
		applyTotal,
		resetTotal,
		cacheHits,
		orphanedAnnotations,
		deviceResolutionFailures,
		recoveredRules,
	)
}
