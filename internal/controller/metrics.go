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
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// iolimiterPods is F8's "no Prometheus signal distinguishes '0 pending'
// from 'all pending'" fix: one gauge per (IOLimiter, state), registered on
// controller-runtime's shared registry so the manager's own metrics server
// serves it unconditionally, mirroring internal/agent/metrics.go's
// convention. Labelled per-IOLimiter (not aggregated) because that's what
// makes it actionable ("which limiter"), and cardinality is bounded by the
// number of IOLimiter objects in the cluster (a user-authored resource,
// not per-pod) times the four fixed states -- sane per the chunk spec's
// instruction.
var iolimiterPods = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "iolimiter_controller_pods",
	Help: "Pods counted per IOLimiter, by state (matched|applied|failed|pending), from the last completed IOLimiterReconciler reconcile.",
}, []string{"iolimiter", "state"})

func init() {
	ctrlmetrics.Registry.MustRegister(iolimiterPods)
}

// setIOLimiterPodsGauge sets iolimiter_controller_pods{iolimiter=key,state=...}
// to an absolute snapshot from the just-completed reconcile (never
// Add/Sub): safe after a manager restart with no memory of prior state,
// matching the agent's own gauge convention.
func setIOLimiterPodsGauge(key string, matched, applied, failed, pending int32) {
	iolimiterPods.WithLabelValues(key, "matched").Set(float64(matched))
	iolimiterPods.WithLabelValues(key, "applied").Set(float64(applied))
	iolimiterPods.WithLabelValues(key, "failed").Set(float64(failed))
	iolimiterPods.WithLabelValues(key, "pending").Set(float64(pending))
}

// deleteIOLimiterPodsGauge removes every state's series for a deleted
// IOLimiter, so a gauge value doesn't outlive the object it describes.
func deleteIOLimiterPodsGauge(key string) {
	for _, state := range []string{"matched", "applied", "failed", "pending"} {
		iolimiterPods.DeleteLabelValues(key, state)
	}
}
