// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"

	"github.com/HYGON-AI/hcu-dcgm/v3/pkg/dcgm"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

func (d *Driver) nodePrepareResource(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("claim %s has no allocation yet", claim.UID),
		}
	}

	if result, ok := d.prepareExistingClaim(claim); ok {
		return result
	}

	devices, created, err := d.prepareAllocatedDevices(ctx, claim)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}

	if len(devices) == 0 {
		klog.V(4).Infof("PrepareResourceClaims: no matching devices for claim %s", claim.UID)
	}

	if err := d.finalizePreparedClaim(claim, created); err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}

	return kubeletplugin.PrepareResult{Devices: devices}
}

func (d *Driver) prepareExistingClaim(claim *resourceapi.ResourceClaim) (kubeletplugin.PrepareResult, bool) {
	d.state.mu.Lock()
	existing := d.state.prepared[claim.UID]
	d.state.mu.Unlock()
	if len(existing) == 0 {
		return kubeletplugin.PrepareResult{}, false
	}

	devices := make([]kubeletplugin.Device, 0, len(existing))
	for _, p := range existing {
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{p.RequestName},
			PoolName:     p.PoolName,
			DeviceName:   p.ParentDevice,
			CDIDeviceIDs: []string{p.CDIDeviceID},
		})
	}

	d.state.mu.Lock()
	var repairErr error
	if d.state.vdeviceCDISpecFilesNeedRepair() {
		repairErr = d.state.syncVDeviceCDISpecFiles("prepare-idempotent-repair")
	}
	if repairErr == nil {
		repairErr = d.state.syncDynamicMarkFiles("prepare-idempotent-repair")
	}
	d.state.mu.Unlock()
	if repairErr != nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("repair prepared artifacts on idempotent prepare: %w", repairErr),
		}, true
	}

	return kubeletplugin.PrepareResult{Devices: devices}, true
}

func (d *Driver) prepareAllocatedDevices(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
) ([]kubeletplugin.Device, []PreparedDevice, error) {
	var devices []kubeletplugin.Device
	var created []PreparedDevice

	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != DriverName {
			continue
		}

		alloc, ok := d.state.allocatable[result.Device]
		if !ok {
			rollbackCreatedVDevices(created)
			return nil, nil, fmt.Errorf("allocatable device not found: %q", result.Device)
		}

		reqCores, reqMemMiB, wholeCard, err := requestCapacityFor(claim, result.Request, alloc)
		if err != nil {
			rollbackCreatedVDevices(created)
			return nil, nil, err
		}

		var prepared PreparedDevice
		var device kubeletplugin.Device
		if wholeCard {
			prepared, device, err = d.prepareWholeCardDevice(ctx, claim, result, alloc)
		} else {
			prepared, device, err = d.prepareVHCUDevice(ctx, claim, result, alloc, reqCores, reqMemMiB)
		}
		if err != nil {
			rollbackCreatedVDevices(created)
			return nil, nil, err
		}

		created = append(created, prepared)
		devices = append(devices, device)
	}

	return devices, created, nil
}

func (d *Driver) prepareWholeCardDevice(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	result resourceapi.DeviceRequestAllocationResult,
	alloc *AllocatableDevice,
) (PreparedDevice, kubeletplugin.Device, error) {
	cdiID := d.state.cdiStandardDeviceID(alloc)
	prepared := PreparedDevice{
		ClaimUID:     claim.UID,
		ParentDevice: result.Device,
		RequestName:  result.Request,
		PoolName:     result.Pool,
		WholeCard:    true,
		CDIName:      physicalCDIDeviceName(alloc),
		CDIDeviceID:  cdiID,
	}
	if err := d.state.attachDynamicMark(ctx, claim, &prepared, alloc); err != nil {
		return PreparedDevice{}, kubeletplugin.Device{}, fmt.Errorf(
			"create dynamic mark for whole-card claim=%s: %w", claim.UID, err)
	}

	device := kubeletplugin.Device{
		Requests:     []string{result.Request},
		PoolName:     result.Pool,
		DeviceName:   result.Device,
		CDIDeviceIDs: []string{cdiID},
	}
	klog.Infof("Whole-card pass-through: claim=%s device=%s cdi=%s", claim.UID, result.Device, cdiID)
	return prepared, device, nil
}

func (d *Driver) prepareVHCUDevice(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	result resourceapi.DeviceRequestAllocationResult,
	alloc *AllocatableDevice,
	reqCores, reqMemMiB int,
) (PreparedDevice, kubeletplugin.Device, error) {
	vdevIDs, err := dcgm.CreateVDevices(alloc.DvInd, 1, []int{reqCores}, []int{reqMemMiB})
	if err != nil || len(vdevIDs) == 0 {
		return PreparedDevice{}, kubeletplugin.Device{}, fmt.Errorf(
			"create vdevice for device=%s dvInd=%d request=%s cores=%d memMiB=%d: %w",
			result.Device, alloc.DvInd, result.Request, reqCores, reqMemMiB, err)
	}
	// Use CreateVDevices' returned id (from /etc/vdev conf diff). Do not use
	// VDeviceSingleInfo().VdvInd: dcgm leaves that field unset (always 0), which
	// made every claim bind CDI vdev0 and left real vdevN untracked.
	vdvInd := vdevIDs[0]

	vInfo, err := dcgm.VDeviceSingleInfo(vdvInd)
	if err != nil {
		_ = dcgm.DestroySingleVDevice(vdvInd)
		return PreparedDevice{}, kubeletplugin.Device{}, fmt.Errorf("query vdevice info vdev=%d: %w", vdvInd, err)
	}

	cdiName := vdeviceCDIName(vdvInd)
	cdiID := d.state.cdiVDeviceID(cdiName)
	prepared := PreparedDevice{
		ClaimUID:     claim.UID,
		ParentDevice: result.Device,
		RequestName:  result.Request,
		PoolName:     result.Pool,
		WholeCard:    false,
		VDvInd:       vdvInd,
		PciBusNumber: vInfo.PciBusNumber,
		CDIName:      cdiName,
		CDIDeviceID:  cdiID,
	}
	if err := d.state.attachDynamicMark(ctx, claim, &prepared, alloc); err != nil {
		_ = dcgm.DestroySingleVDevice(vdvInd)
		return PreparedDevice{}, kubeletplugin.Device{}, fmt.Errorf(
			"create dynamic mark for vHCU claim=%s: %w", claim.UID, err)
	}

	device := kubeletplugin.Device{
		Requests:     []string{result.Request},
		PoolName:     result.Pool,
		DeviceName:   result.Device,
		CDIDeviceIDs: []string{cdiID},
	}
	klog.Infof("vHCU created: claim=%s device=%s vdev=%d cores=%d memMiB=%d cdi=%s",
		claim.UID, result.Device, vdvInd, reqCores, reqMemMiB, cdiID)
	return prepared, device, nil
}

func (d *Driver) finalizePreparedClaim(claim *resourceapi.ResourceClaim, created []PreparedDevice) error {
	d.state.mu.Lock()
	d.state.prepared[claim.UID] = created
	err := d.state.syncVDeviceCDISpecFiles("prepare")
	if err == nil {
		err = d.state.syncDynamicMarkFiles("prepare")
	}
	if err == nil {
		if saveErr := d.state.savePreparedState(); saveErr != nil {
			klog.Warningf("save prepared state: %v", saveErr)
		}
	}
	d.state.mu.Unlock()
	if err == nil {
		return nil
	}

	removePreparedDynamicMarks(created)
	rollbackCreatedVDevices(created)
	d.state.mu.Lock()
	delete(d.state.prepared, claim.UID)
	_ = d.state.syncVDeviceCDISpecFiles("prepare-rollback")
	_ = d.state.syncDynamicMarkFiles("prepare-rollback")
	_ = d.state.savePreparedState()
	d.state.mu.Unlock()
	return fmt.Errorf("sync prepared artifacts: %w", err)
}
