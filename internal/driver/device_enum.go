// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"os"

	dcgm "github.com/HYGON-AI/hcu-dcgm/v3/pkg/dcgm"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

func preparedVDvIndSet(prepared map[types.UID][]PreparedDevice) map[int]struct{} {
	tracked := make(map[int]struct{})
	for _, list := range prepared {
		for _, p := range list {
			if p.WholeCard {
				continue
			}
			tracked[p.VDvInd] = struct{}{}
		}
	}
	return tracked
}

// preparedParentDeviceNames returns ResourceSlice device names that currently have a
// prepared claim (vHCU or whole-card). Those parents must stay published so the
// scheduler can allocate remaining consumable capacity to later claims.
func preparedParentDeviceNames(prepared map[types.UID][]PreparedDevice) map[string]struct{} {
	tracked := make(map[string]struct{})
	for _, list := range prepared {
		for _, p := range list {
			if p.ParentDevice == "" {
				continue
			}
			tracked[p.ParentDevice] = struct{}{}
		}
	}
	return tracked
}

// reconcileStaleHostVDevices destroys on-host vHCUs that are not tracked in prepared state.
// Without this, untracked VDeviceCount!=0 would leave cards unpublished after restart.
func reconcileStaleHostVDevices(prepared map[types.UID][]PreparedDevice) {
	tracked := preparedVDvIndSet(prepared)
	vinfos, err := dcgm.VDeviceInfos()
	if err != nil {
		klog.Warningf("reconcile stale vHCU: VDeviceInfos: %v", err)
		return
	}
	var destroyed int
	for _, v := range vinfos {
		if _, ok := tracked[v.VdvInd]; ok {
			continue
		}
		if err := dcgm.DestroySingleVDevice(v.VdvInd); err != nil {
			klog.Warningf("reconcile stale vHCU: destroy vdev=%d dvInd=%d: %v", v.VdvInd, v.DvInd, err)
			continue
		}
		confPath := fmt.Sprintf("/etc/vdev/vdev%d.conf", v.VdvInd)
		_ = os.Remove(confPath)
		klog.Infof("reconcile stale vHCU: destroyed untracked vdev=%d (dvInd=%d), removed %s", v.VdvInd, v.DvInd, confPath)
		destroyed++
	}
	if destroyed > 0 {
		klog.Infof("reconcile stale vHCU: destroyed %d untracked virtual device(s)", destroyed)
	}
}

func buildAllocatableFromDeviceInfos(deviceInfos []dcgm.DeviceInfo, prepared map[types.UID][]PreparedDevice) map[string]*AllocatableDevice {
	trackedParents := preparedParentDeviceNames(prepared)
	allocatable := make(map[string]*AllocatableDevice)
	for _, di := range deviceInfos {
		name := canonicalNameFromPCI(di.PciBusNumber)
		if di.VDeviceCount != 0 {
			if _, ok := trackedParents[name]; !ok {
				// Untracked host vHCUs: do not publish until reconcile removes them.
				klog.Infof("skip allocatable device pci=%s dvInd=%d: host has %d untracked vHCU(s); "+
					"card stays unpublished until stale vHCUs are destroyed",
					di.PciBusNumber, di.DvInd, di.VDeviceCount)
				continue
			}
			// Tracked vHCUs consume capacity via DRA allocations; keep publishing the
			// parent so later claims can share remaining cores/memory on this card.
			klog.V(4).Infof("publish allocatable device pci=%s dvInd=%d with %d tracked vHCU(s) for consumable sharing",
				di.PciBusNumber, di.DvInd, di.VDeviceCount)
		}

		info := DeviceInfo{
			DvInd:             di.DvInd,
			PciBusNumber:      di.PciBusNumber,
			DeviceId:          di.DeviceId,
			DevTypeName:       di.DevTypeName,
			SubsystemTypeName: "",
			ComputeUnit:       di.ComputeUnit,
			MemoryTotal:       di.MemoryTotal,
		}

		alloc, err := newAllocatableDevice(info)
		if err != nil {
			klog.Warningf("skip device pci=%s: %v", di.PciBusNumber, err)
			continue
		}
		allocatable[alloc.Name] = alloc
	}
	return allocatable
}

// RefreshAllocatable rebuilds allocatable devices and the base CDI spec.
func (s *DeviceState) RefreshAllocatable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshAllocatable()
}

// refreshAllocatable rebuilds allocatable devices and the base CDI spec. Caller must hold s.mu.
func (s *DeviceState) refreshAllocatable() error {
	reconcileStaleHostVDevices(s.prepared)

	deviceInfos, err := dcgm.DeviceInfos()
	if err != nil {
		return fmt.Errorf("dcgm.DeviceInfos: %w", err)
	}
	s.allocatable = buildAllocatableFromDeviceInfos(deviceInfos, s.prepared)
	klog.Infof("Refreshed allocatable devices: %d", len(s.allocatable))
	return s.createStandardCDISpecFile()
}
