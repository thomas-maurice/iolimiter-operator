// k8s-blkio-limiter: a Kubernetes DaemonSet that applies cgroup v2 io.max limits
// to containers based on pod annotations.
//
// See README.md and instructions.md for usage.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/thomas-maurice/k8s-blkio-limiter/internal/limiter"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	hostCgroupRoot = "/host-cgroup"
	hostProcRoot   = "/host-proc"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
	slog.SetDefault(logger)

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		logger.Error("NODE_NAME env var is required (set via Downward API)")
		os.Exit(1)
	}
	logger.Info("starting k8s-blkio-limiter", "node", nodeName)

	if _, err := os.Stat(filepath.Join(hostCgroupRoot, "cgroup.controllers")); err != nil {
		logger.Error("cgroups v2 not detected on host", "err", err)
		os.Exit(1)
	}
	logger.Info("cgroups v2 confirmed on host")

	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Error("failed to get in-cluster config", "err", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Error("failed to create kubernetes client", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	go func() {
		logger.Info("starting metrics server", "addr", ":8080")
		if err := http.ListenAndServe(":8080", mux); err != nil {
			logger.Error("metrics server failed", "err", err)
			os.Exit(1)
		}
	}()

	l := limiter.New(hostCgroupRoot, hostProcRoot, nodeName, clientset, logger)
	l.Run(context.Background())
}
