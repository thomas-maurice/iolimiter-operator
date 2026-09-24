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

package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// SPEC.md §6.1's metrics table, registered on controller-runtime's shared
// registry so the manager's own metrics server serves them unconditionally.
var (
	ownedDevices = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "iolimiter_agent_owned_devices",
		Help: "Devices this agent currently owns a rule for, across every PodIOLimit on this node.",
	})

	kernelWritesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iolimiter_agent_kernel_writes_total",
		Help: "io.max write(2) calls, by operation (apply|reset) and result (ok|error).",
	}, []string{"op", "result"})

	driftCorrectionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "iolimiter_agent_drift_corrections_total",
		Help: "io.max rules rewritten because the kernel's line no longer matched the last-applied rule.",
	})

	volumeResolutionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "iolimiter_agent_volume_resolution_total",
		Help: "Volume resolutions, by result (Applied|Pending|Unsupported|Failed reason, e.g. NoBlockDevice).",
	}, []string{"result"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(ownedDevices, kernelWritesTotal, driftCorrectionsTotal, volumeResolutionTotal)
}

// recordKernelWrite increments iolimiter_agent_kernel_writes_total{op,result}.
func recordKernelWrite(op string, err error) {
	result := "ok"
	if err != nil {
		result = "error"
	}
	kernelWritesTotal.WithLabelValues(op, result).Inc()
}

// recordDriftCorrection increments iolimiter_agent_drift_corrections_total.
func recordDriftCorrection() {
	driftCorrectionsTotal.Inc()
}

// recordVolumeResolution increments iolimiter_agent_volume_resolution_total{result}.
func recordVolumeResolution(result string) {
	volumeResolutionTotal.WithLabelValues(result).Inc()
}

// setOwnedDevicesGauge sets iolimiter_agent_owned_devices to count. Callers
// pass an absolute count derived from a fresh read (typically summed across
// every PodIOLimit's status.devices seen so far), matching the reference
// project's "absolute Set, not Add/Sub" convention: safe after a restart
// with no memory of prior state.
func setOwnedDevicesGauge(count int) {
	ownedDevices.Set(float64(count))
}
