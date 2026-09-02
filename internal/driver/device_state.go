// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"

	dcgm "github.com/HYGON-AI/hcu-dcgm/v3/pkg/dcgm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

func NewDeviceState(ctx context.Context, opts DeviceStateOptions) (*DeviceState, error) {
	_ = ctx

	if opts.DriverName == "" {
		return nil, fmt.Errorf("driverName is required")
	}
	if opts.CDIRoot == "" {
		return nil, fmt.Errorf("cdiRoot is required")
	}

	// Initialize DCGM for device enumeration.
	if err := dcgm.Init(); err != nil {
		return nil, fmt.Errorf("dcgm.Init(): %w", err)
	}

	sf := stateFilePath(opts.CDIRoot)
	prepared, err := loadPreparedState(sf)
	if err != nil {
		_ = dcgm.ShutDown()
		return nil, fmt.Errorf("load prepared state: %w", err)
	}
	if len(prepared) > 0 {
		klog.Infof("Restored %d prepared claim(s) from %s", len(prepared), sf)
	}

	// Drop vHCUs left on the host after force-delete / driver crash, then re-enumerate
	// so VDeviceCount reflects the post-reconcile host state.
	reconcileStaleHostVDevices(prepared)
	deviceInfos, err := dcgm.DeviceInfos()
	if err != nil {
		_ = dcgm.ShutDown()
		return nil, fmt.Errorf("dcgm.DeviceInfos(): %w", err)
	}
	allocatable := buildAllocatableFromDeviceInfos(deviceInfos, prepared)

	if err := ensureVDevDynamicDir(); err != nil {
		_ = dcgm.ShutDown()
		return nil, fmt.Errorf("ensure vdev dynamic dir: %w", err)
	}

	state := &DeviceState{
		allocatable: allocatable,
		cdiRoot:     opts.CDIRoot,
		cdiVendor:   opts.DriverName,
		nodeName:    opts.NodeName,
		clientset:   opts.Clientset,
		stateFile:   sf,
		prepared:    prepared,
	}

	if err := state.createStandardCDISpecFile(); err != nil {
		_ = dcgm.ShutDown()
		return nil, fmt.Errorf("create CDI spec: %w", err)
	}

	// Rebuild vDevice CDI spec and dynamic marks from restored state.
	if len(prepared) > 0 {
		if err := state.syncVDeviceCDISpecFiles("state-restore"); err != nil {
			klog.Warningf("sync vDevice CDI after state restore: %v", err)
		}
		if err := state.syncDynamicMarkFiles("state-restore"); err != nil {
			klog.Warningf("sync vdev dynamic marks after state restore: %v", err)
		}
	}

	klog.Infof("Initialized HCU DRA driver: %d allocatable device(s)", len(state.allocatable))
	return state, nil
}

func (s *DeviceState) Shutdown() error {
	return dcgm.ShutDown()
}

// CleanupOrphanPreparedClaims checks all prepared claims against the API server.
// If a ResourceClaim no longer exists (e.g. pod was force-deleted), destroy any
// associated vHCUs and remove the entry from prepared state.
func (s *DeviceState) CleanupOrphanPreparedClaims(ctx context.Context, clientset kubernetes.Interface) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.prepared) == 0 {
		return
	}

	// List all ResourceClaims once and build a UID lookup set.
	claims, err := clientset.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Warningf("orphan cleanup: failed to list ResourceClaims: %v", err)
		return
	}
	existingUIDs := make(map[types.UID]bool, len(claims.Items))
	for i := range claims.Items {
		existingUIDs[claims.Items[i].UID] = true
	}

	// Find orphan UIDs: prepared but no longer exist in the cluster.
	var orphanUIDs []types.UID
	for uid := range s.prepared {
		if !existingUIDs[uid] {
			orphanUIDs = append(orphanUIDs, uid)
		}
	}

	// Clean up orphan vHCUs and dynamic marks.
	for _, uid := range orphanUIDs {
		prepared := s.prepared[uid]
		removePreparedDynamicMarks(prepared)
		delete(s.prepared, uid)
		for _, p := range prepared {
			if p.WholeCard {
				klog.Infof("orphan cleanup: released whole-card claim=%s device=%s", uid, p.ParentDevice)
				continue
			}
			if err := dcgm.DestroySingleVDevice(p.VDvInd); err != nil {
				klog.Warningf("orphan cleanup: destroy vdevice failed claim=%s vdev=%d: %v", uid, p.VDvInd, err)
			} else {
				klog.Infof("orphan cleanup: destroyed orphan vHCU claim=%s vdev=%d", uid, p.VDvInd)
			}
		}
	}

	if len(orphanUIDs) > 0 {
		if err := s.syncVDeviceCDISpecFiles("orphan-cleanup"); err != nil {
			klog.Warningf("orphan cleanup: sync CDI spec: %v", err)
		}
		if err := s.syncDynamicMarkFiles("orphan-cleanup"); err != nil {
			klog.Warningf("orphan cleanup: sync dynamic marks: %v", err)
		}
		if err := s.savePreparedState(); err != nil {
			klog.Warningf("orphan cleanup: save state: %v", err)
		}
		klog.Infof("orphan cleanup: cleaned up %d orphan claim(s)", len(orphanUIDs))
	}
}
