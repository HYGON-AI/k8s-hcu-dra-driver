// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/logs"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	"github.com/HYGON-AI/k8s-hcu-dra-driver/internal/driver"
)

const (
	// LOG_LEVEL sets klog verbosity (-v). Allowed range 1–5 (see deployment manifest).
	logLevelEnv = "LOG_LEVEL"
	logLevelMin = 1
	logLevelMax = 5
)

func main() {
	logs.InitLogs()
	if err := applyLogLevelFromEnv(); err != nil {
		klog.Fatalf("%v", err)
	}
	flag.Parse()

	nodeName := getenvDefault("NODE_NAME", "")
	namespace := getenvDefault("NAMESPACE", "default")
	cdiRoot := getenvDefault("CDI_ROOT", "/var/run/cdi")
	kubeletRegistrarDir := getenvDefault("KUBELET_REGISTRAR_DIRECTORY_PATH", kubeletplugin.KubeletRegistryDir)
	kubeletPluginsDir := getenvDefault("KUBELET_PLUGINS_DIRECTORY_PATH", kubeletplugin.KubeletPluginsDir)

	if nodeName == "" {
		klog.Fatal("missing required env NODE_NAME")
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("failed to create in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		klog.Fatalf("failed to create kubernetes clientset: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	state, err := driver.NewDeviceState(ctx, driver.DeviceStateOptions{
		NodeName:   nodeName,
		DriverName: driver.DriverName,
		Namespace:  namespace,
		CDIRoot:    cdiRoot,
		Clientset:  clientset,
	})
	if err != nil {
		klog.Fatalf("failed to initialize device state: %v", err)
	}
	defer func() {
		if err := state.Shutdown(); err != nil {
			klog.Warningf("shutdown state: %v", err)
		}
	}()

	drv := driver.NewDriver(state)

	// Ensure the kubelet plugin registration directory exists.
	_ = os.MkdirAll(kubeletRegistrarDir, 0755)
	_ = os.MkdirAll(kubeletPluginsDir, 0755)

	plugin, err := kubeletplugin.Start(
		ctx,
		drv,
		kubeletplugin.KubeClient(clientset),
		kubeletplugin.NodeName(nodeName),
		kubeletplugin.DriverName(driver.DriverName),
		// Serialize Prepare/Unprepare to avoid concurrent CDI rewrites racing with vHCU creation.
		kubeletplugin.Serialize(true),
		kubeletplugin.RegistrarDirectoryPath(kubeletRegistrarDir),
		kubeletplugin.PluginDataDirectoryPath(kubeletPluginsDir),
	)
	if err != nil {
		klog.Fatalf("start kubelet plugin: %v", err)
	}

	// Clean up orphan prepared claims from previous force-deleted pods.
	state.CleanupOrphanPreparedClaims(ctx, clientset)

	// Re-enumerate after orphan cleanup (vHCU destroy) so ResourceSlice is not stuck at 0 devices.
	if err := state.RefreshAllocatable(); err != nil {
		klog.Fatalf("refresh allocatable after orphan cleanup: %v", err)
	}

	if err := driver.PublishResources(ctx, plugin, state, nodeName); err != nil {
		klog.Fatalf("publish resources: %v", err)
	}

	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				state.CleanupOrphanPreparedClaims(ctx, clientset)
				if err := state.RefreshAllocatable(); err != nil {
					klog.Warningf("periodic refresh allocatable: %v", err)
				}
				if err := driver.PublishResources(ctx, plugin, state, nodeName); err != nil {
					klog.Warningf("periodic publish resources: %v", err)
				}
			}
		}
	}()

	<-ctx.Done()

	// Shutdown is handled by kubeletplugin once context is cancelled.
	_ = plugin
	klog.Info("hcu dra driver stopped")
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func applyLogLevelFromEnv() error {
	s := os.Getenv(logLevelEnv)
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%s must be an integer between %d and %d: %w", logLevelEnv, logLevelMin, logLevelMax, err)
	}
	if n < logLevelMin || n > logLevelMax {
		return fmt.Errorf("%s must be between %d and %d, got %d", logLevelEnv, logLevelMin, logLevelMax, n)
	}
	if err := flag.Set("v", s); err != nil {
		return fmt.Errorf("set klog -v from %s: %w", logLevelEnv, err)
	}
	return nil
}
