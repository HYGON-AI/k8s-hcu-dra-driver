// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"
	"math"

	"github.com/HYGON-AI/hcu-dcgm/v3/pkg/dcgm"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	apiruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
)

// Driver implements the DRA kubelet plugin callbacks.
type Driver struct {
	state *DeviceState
}

// NewDriver creates a DRA kubelet plugin driver backed by DeviceState.
func NewDriver(state *DeviceState) *Driver {
	return &Driver{state: state}
}

func (d *Driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	klog.V(4).Infof("PrepareResourceClaims called with %d claim(s)", len(claims))

	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		results[claim.UID] = d.nodePrepareResource(ctx, claim)
	}
	return results, nil
}

func (d *Driver) UnprepareResourceClaims(
	ctx context.Context,
	claimRefs []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	klog.V(4).Infof("UnprepareResourceClaims called with %d claimRef(s)", len(claimRefs))

	results := make(map[types.UID]error, len(claimRefs))
	for _, claimRef := range claimRefs {
		results[claimRef.UID] = d.nodeUnprepareResource(ctx, claimRef)
	}
	return results, nil
}

func (d *Driver) HandleError(ctx context.Context, err error, msg string) {
	apiruntime.HandleErrorWithContext(ctx, err, msg)
}

func (d *Driver) Shutdown() error {
	return nil
}

func (d *Driver) nodeUnprepareResource(_ context.Context, claimRef kubeletplugin.NamespacedObject) error {
	d.state.mu.Lock()
	prepared := d.state.prepared[claimRef.UID]
	removePreparedDynamicMarks(prepared)
	delete(d.state.prepared, claimRef.UID)
	if err := d.state.syncVDeviceCDISpecFiles("unprepare"); err != nil {
		klog.Warningf("sync vdevice CDI spec during unprepare claim=%s: %v", claimRef.UID, err)
	}
	if err := d.state.syncDynamicMarkFiles("unprepare"); err != nil {
		klog.Warningf("sync vdev dynamic marks during unprepare claim=%s: %v", claimRef.UID, err)
	}
	if err := d.state.savePreparedState(); err != nil {
		klog.Warningf("save prepared state during unprepare claim=%s: %v", claimRef.UID, err)
	}
	d.state.mu.Unlock()

	// Prepare may have created fallback marks without recording prepared state (e.g. crash
	// or claim deleted mid-Prepare). Always sweep by claim UID.
	removeDynamicMarksByClaimUID(string(claimRef.UID))

	for _, p := range prepared {
		if p.WholeCard {
			klog.Infof("Whole-card released: claim=%s device=%s (no vHCU to destroy)", claimRef.UID, p.ParentDevice)
			continue
		}
		if err := dcgm.DestroySingleVDevice(p.VDvInd); err != nil {
			klog.Warningf("destroy vdevice failed claim=%s vdev=%d: %v", claimRef.UID, p.VDvInd, err)
		}
	}
	return nil
}

func rollbackCreatedVDevices(created []PreparedDevice) {
	removePreparedDynamicMarks(created)
	for _, p := range created {
		if p.WholeCard {
			continue // nothing to destroy for whole-card
		}
		if err := dcgm.DestroySingleVDevice(p.VDvInd); err != nil {
			klog.Warningf("rollback destroy vdevice failed claim=%s vdev=%d: %v", p.ClaimUID, p.VDvInd, err)
		}
	}
}

// requestCapacityFor extracts cores and memory from a claim's device request.
// wholeCard is true when the user did NOT specify any capacity (hcucores/hcumem),
// meaning the entire physical card should be passed through without creating a vHCU.
func requestCapacityFor(claim *resourceapi.ResourceClaim, requestName string, alloc *AllocatableDevice) (cores int, memMiB int, wholeCard bool, err error) {
	var req *resourceapi.DeviceRequest
	for i := range claim.Spec.Devices.Requests {
		if claim.Spec.Devices.Requests[i].Name == requestName {
			req = &claim.Spec.Devices.Requests[i]
			break
		}
	}
	if req == nil || req.Exactly == nil {
		return 0, 0, false, fmt.Errorf("request %q not found in claim %s spec", requestName, claim.UID)
	}

	capReq := map[resourceapi.QualifiedName]resource.Quantity{}
	if req.Exactly.Capacity != nil {
		capReq = req.Exactly.Capacity.Requests
	}

	_, hasC := capReq["cores"]
	_, hasM := capReq["memory"]

	// If neither cores nor memory is specified, this is a whole-card pass-through.
	if !hasC && !hasM {
		return 0, 0, true, nil
	}

	// vHCU mode: use requested values, fall back to device maximum for the missing one.
	cores = int(alloc.ComputeUnits)
	if hasC {
		c := capReq["cores"]
		cores = int(math.Max(1, float64(c.Value())))
	}

	memMiB = int(alloc.MemoryBytes / (1024 * 1024))
	if hasM {
		m := capReq["memory"]
		memMiB = int(math.Max(1, float64(m.Value()/(1024*1024))))
	}
	if cores < 1 {
		cores = 1
	}
	if memMiB < 1 {
		memMiB = 1
	}

	return cores, memMiB, false, nil
}

func PublishResources(ctx context.Context, helper *kubeletplugin.Helper, state *DeviceState, nodeName string) error {
	var resourceSlice resourceslice.Slice
	for _, device := range state.allocatable {
		resourceSlice.Devices = append(resourceSlice.Devices, device.GetDevice())
	}

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {Slices: []resourceslice.Slice{resourceSlice}},
		},
	}

	return helper.PublishResources(ctx, resources)
}
