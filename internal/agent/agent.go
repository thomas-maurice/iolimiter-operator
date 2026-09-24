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
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	storagev1alpha1 "github.com/thomas-maurice/k8s-blkio-limiter/api/v1alpha1"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/blockdev"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/cgroup"
)

// sysRoot is where the container's own sysfs is mounted (D14: block
// devices aren't namespaced, so no host bind mount is needed for
// /sys/dev/block). Not a flag: SPEC.md §6.1 lists no --sys-root override.
const sysRoot = "/sys"

var setupLog = ctrl.Log.WithName("agent-setup")

// defaultProtectedPaths is D34's default --protected-paths value: the
// node's root filesystem plus kubelet's and the common container runtimes'
// storage roots (all of them can share a disk with a limited pod's
// volume, K7).
const defaultProtectedPaths = "/,/var/lib/kubelet,/var/lib/containerd,/var/lib/containers"

// config holds agent.Main's parsed flags.
type config struct {
	nodeName          string
	cgroupRoot        string
	procRoot          string
	kubeletRootDir    string
	kubepodsOverride  string
	resyncPeriod      time.Duration
	protectedPathsRaw string
	protectedPaths    []string
	allowRootDevice   bool
	metricsAddr       string
	probeAddr         string
	secureMetrics     bool
	metricsCertPath   string
	metricsCertName   string
	metricsCertKey    string
	enableHTTP2       bool
}

func parseFlags(args []string) (*config, *zap.Options, error) {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)

	cfg := &config{}
	fs.StringVar(&cfg.nodeName, "node-name", os.Getenv("NODE_NAME"), "The node this agent enforces PodIOLimits for.")
	fs.StringVar(&cfg.cgroupRoot, "cgroup-root", "/host/cgroup", "Host cgroup2 mount, bind-mounted read-write (D14).")
	fs.StringVar(&cfg.procRoot, "proc-root", "/host/proc", "Host /proc, bind-mounted read-only (D14); only <proc-root>/1/mountinfo is read.")
	fs.StringVar(&cfg.kubeletRootDir, "kubelet-root-dir", "/var/lib/kubelet", "Kubelet's root directory, as seen from the host mountinfo.")
	fs.StringVar(&cfg.kubepodsOverride, "kubepods-cgroup", "", "Override kubepods root discovery (D6) with this path, relative to --cgroup-root.")
	fs.DurationVar(&cfg.resyncPeriod, "resync-period", 60*time.Second, "How often to re-check applied devices for drift (D11).")
	fs.StringVar(&cfg.protectedPathsRaw, "protected-paths", defaultProtectedPaths, "Comma-separated host paths whose whole-disk device is refused unless --allow-root-device (D34); missing paths are ignored.")
	fs.BoolVar(&cfg.allowRootDevice, "allow-root-device", false, "Allow throttling a device backing --protected-paths (D34).")
	fs.StringVar(&cfg.metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. Use :8443 for HTTPS, or 0 to disable.")
	fs.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	fs.BoolVar(&cfg.secureMetrics, "metrics-secure", true, "Serve metrics via HTTPS with authn/authz. --metrics-secure=false for HTTP.")
	fs.StringVar(&cfg.metricsCertPath, "metrics-cert-path", "", "Directory containing the metrics server certificate.")
	fs.StringVar(&cfg.metricsCertName, "metrics-cert-name", "tls.crt", "Metrics server certificate file name.")
	fs.StringVar(&cfg.metricsCertKey, "metrics-cert-key", "tls.key", "Metrics server key file name.")
	fs.BoolVar(&cfg.enableHTTP2, "enable-http2", false, "Enable HTTP/2 for the metrics server.")

	opts := &zap.Options{
		Development: true,
		Level:       zapcore.InfoLevel,
	}
	opts.BindFlags(fs)

	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	if cfg.nodeName == "" {
		return nil, nil, fmt.Errorf("--node-name is required (or set NODE_NAME)")
	}
	cfg.protectedPaths = splitProtectedPaths(cfg.protectedPathsRaw)
	return cfg, opts, nil
}

// splitProtectedPaths parses --protected-paths' comma-separated value,
// trimming whitespace and dropping empty entries (so a trailing comma or
// "--protected-paths=" doesn't turn into a spurious "" path).
func splitProtectedPaths(raw string) []string {
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Main runs the `agent` subcommand. args excludes argv[0] and the "agent"
// word (D12). It returns a process exit code.
func Main(ctx context.Context, args []string) int {
	cfg, opts, err := parseFlags(args)
	if err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(opts)))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(storagev1alpha1.AddToScheme(scheme))

	layout, err := cgroup.DiscoverKubepods(cfg.cgroupRoot, cfg.kubepodsOverride)
	if err != nil {
		setupLog.Error(err, "Failed to discover kubepods cgroup root")
		return 1
	}
	// D28: --cgroup-root is only bind-mounted down to the kubepods subtree
	// (config/agent/agent.yaml, hack/helm-postprocess.sh), so cfg.cgroupRoot
	// itself is an empty directory on a narrowed mount -- cgroup.controllers
	// only exists inside the actual mounted subtree. cgroup2 exposes
	// cgroup.controllers in every directory of the hierarchy, not just the
	// real mount point, so checking it at layout.Base (rather than
	// cfg.cgroupRoot) is still an exact "is this really cgroup2" proof,
	// verified on kind.
	if _, statErr := os.Stat(filepath.Join(layout.Base, "cgroup.controllers")); statErr != nil {
		setupLog.Error(statErr, "cgroup2 not mounted under the discovered kubepods root", "kubepodsRoot", layout.Base)
		return 1
	}
	if ok, ioErr := ioControllerInSubtree(layout.Base); ioErr != nil || !ok {
		setupLog.Error(ioErr, "io controller not enabled in kubepods cgroup.subtree_control", "kubepodsRoot", layout.Base)
		return 1
	}
	// F13 part 2: "io" in cgroup.subtree_control only proves the io
	// subsystem (CONFIG_BLK_CGROUP) is enabled, not that the kernel was
	// built with CONFIG_BLK_DEV_THROTTLING -- without it, io.max never
	// appears anywhere (io.stat still does), and every pod would otherwise
	// report a misleading Failed/IOControllerDisabled instead of this
	// clear, fail-fast startup error. layout.Base itself has io.max iff
	// its own parent enabled "io" for it and the controller's io.max
	// cftype is registered, so this check needs no pod cgroup to exist yet.
	if !ioMaxPresentInSubtree(layout.Base) {
		setupLog.Error(nil, "io controller enabled but no io.max file found under the kubepods root (kernel likely missing CONFIG_BLK_DEV_THROTTLING)", "kubepodsRoot", layout.Base)
		return 1
	}
	entries, mErr := readMountinfo(cfg.procRoot)
	if mErr != nil {
		setupLog.Error(mErr, "Failed to read host mountinfo", "procRoot", cfg.procRoot)
		return 1
	}

	// D34: resolve --protected-paths to their whole-disk devices once at
	// startup and log the set (D26); missing paths (a node without e.g.
	// /var/lib/containerd) are silently omitted, not an error.
	protectedDevices := blockdev.ResolveProtectedDevices(sysRoot, entries, cfg.protectedPaths)
	setupLog.Info("Resolved protected-device set", "protectedPaths", cfg.protectedPaths, "protectedDevices", protectedDevices, "allowRootDevice", cfg.allowRootDevice)

	setupLog.Info("Agent starting", "node", cfg.nodeName, "cgroupRoot", cfg.cgroupRoot, "procRoot", cfg.procRoot,
		"kubeletRootDir", cfg.kubeletRootDir, "kubepodsRoot", layout.Base, "driver", layout.Driver, "prefix", layout.Prefix,
		"resyncPeriod", cfg.resyncPeriod, "allowRootDevice", cfg.allowRootDevice)

	var tlsOpts []func(*tls.Config)
	if !cfg.enableHTTP2 {
		tlsOpts = append(tlsOpts, func(c *tls.Config) { c.NextProtos = []string{"http/1.1"} })
	}
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.metricsAddr,
		SecureServing: cfg.secureMetrics,
		TLSOpts:       tlsOpts,
	}
	if cfg.secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}
	if cfg.metricsCertPath != "" {
		metricsServerOptions.CertDir = cfg.metricsCertPath
		metricsServerOptions.CertName = cfg.metricsCertName
		metricsServerOptions.KeyName = cfg.metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			// D16: the agent's cache holds nothing but this node's
			// PodIOLimits. No other type is ever read through the client.
			ByObject: map[client.Object]cache.ByObject{
				&storagev1alpha1.PodIOLimit{}: {
					Field: fields.OneTermEqualSelector("spec.nodeName", cfg.nodeName),
				},
			},
		},
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: cfg.probeAddr,
		LeaderElection:         false, // D11: event-driven per node, no leader election.
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		return 1
	}

	h := newHealth(cfg.resyncPeriod)
	h.recordSelfCheck(true) // the startup checks above already passed.

	reconciler := &PodIOLimitReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Recorder:        mgr.GetEventRecorder("k8s-blkio-limiter-agent"),
		NodeName:        cfg.nodeName,
		Layout:          layout,
		CgroupRoot:      cfg.cgroupRoot,
		ProcRoot:        cfg.procRoot,
		KubeletRootDir:  cfg.kubeletRootDir,
		SysRoot:         sysRoot,
		ResyncPeriod:    cfg.resyncPeriod,
		ProtectedPaths:  cfg.protectedPaths,
		AllowRootDevice: cfg.allowRootDevice,
		Health:          h,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "podiolimit")
		return 1
	}

	go runSelfCheckLoop(ctx, h, cfg, layout.Base)

	go func() {
		if mgr.GetCache().WaitForCacheSync(ctx) {
			h.recordCacheSynced(true)
			setupLog.Info("Cache synced")
		}
	}()

	if err := mgr.AddHealthzCheck("healthz", h.livezCheck); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		return 1
	}
	if err := mgr.AddReadyzCheck("readyz", h.readyzCheck); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		return 1
	}

	setupLog.Info("Starting agent manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Agent manager exited with an error")
		return 1
	}
	return 0
}

// runSelfCheckLoop periodically re-verifies D24's self-check preconditions
// (cgroup root + kubepods dir present, proc-root mountinfo readable) so
// livezCheck has a fresh signal to compare against, not just the one-time
// startup pass.
func runSelfCheckLoop(ctx context.Context, h *health, cfg *config, kubepodsRoot string) {
	ticker := time.NewTicker(cfg.resyncPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.recordSelfCheck(selfCheckPasses(cfg, kubepodsRoot))
		}
	}
}

// selfCheckPasses re-checks cgroup2's presence at kubepodsRoot, not
// cfg.cgroupRoot (D28: cfg.cgroupRoot is only bind-mounted down to the
// kubepods subtree, so it has no cgroup.controllers of its own -- see
// Main's matching comment).
func selfCheckPasses(cfg *config, kubepodsRoot string) bool {
	if _, err := os.Stat(filepath.Join(kubepodsRoot, "cgroup.controllers")); err != nil {
		return false
	}
	if _, err := readMountinfo(cfg.procRoot); err != nil {
		return false
	}
	return true
}

// ioControllerInSubtree reports whether "io" is enabled in
// <kubepodsRoot>/cgroup.subtree_control (required for io.max to ever
// appear in any pod cgroup under it).
func ioControllerInSubtree(kubepodsRoot string) (bool, error) {
	content, err := os.ReadFile(filepath.Join(kubepodsRoot, "cgroup.subtree_control"))
	if err != nil {
		return false, err
	}
	if slices.Contains(strings.Fields(string(content)), "io") {
		return true, nil
	}
	return false, nil
}

// ioMaxPresentInSubtree reports whether kubepodsRoot itself has an io.max
// file (F13 part 2): distinct from ioControllerInSubtree, which only
// proves the io subsystem is enabled for kubepodsRoot's children, not that
// io.max (registered by the blk-throttle policy, CONFIG_BLK_DEV_THROTTLING)
// exists at all on this kernel.
func ioMaxPresentInSubtree(kubepodsRoot string) bool {
	_, err := os.Stat(filepath.Join(kubepodsRoot, "io.max"))
	return err == nil
}
