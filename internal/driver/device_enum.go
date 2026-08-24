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

// reconcileStaleHostVDevices destroys on-host vHCUs that are not tracked in prepared state.
// Without this, VDeviceCount!=0 causes all physical cards to be skipped and allocatable=0 after restart.
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

func buildAllocatableFromDeviceInfos(deviceInfos []dcgm.DeviceInfo) map[string]*AllocatableDevice {
	allocatable := make(map[string]*AllocatableDevice)
	for _, di := range deviceInfos {
		if di.VDeviceCount != 0 {
			klog.Infof("skip allocatable device pci=%s dvInd=%d: host already has %d vHCU(s); "+
				"only empty physical cards are published (stale vHCU is removed on startup if not in prepared state)",
				di.PciBusNumber, di.DvInd, di.VDeviceCount)
			continue
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
	s.allocatable = buildAllocatableFromDeviceInfos(deviceInfos)
	klog.Infof("Refreshed allocatable devices: %d", len(s.allocatable))
	return s.createStandardCDISpecFile()
}
