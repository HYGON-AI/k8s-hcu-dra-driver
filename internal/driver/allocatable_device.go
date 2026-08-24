// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"math"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	hcuDeviceType = "hcu"

	attributeTypeKey         = "type"
	attributeUUIDKey         = "uuid"
	attributeProductNameKey  = "productName"
	attributeArchitectureKey = "architecture"
	attributeBrandKey        = "brand"

	capacityCoresKey  = "cores"
	capacityMemoryKey = "memory"
)

type AllocatableDevice struct {
	Name string // canonical ResourceSlice device name (PCI-based); CDI name uses UUID

	// Hardware identity
	DvInd        int
	PciBusNumber string
	UUID         string
	ProductName  string
	Architecture string
	Brand        string

	// Capacity
	ComputeUnits int64
	MemoryBytes  int64

	// For CDI device injection
	RenderCardNames []string
}

func newAllocatableDevice(di DeviceInfo) (*AllocatableDevice, error) {
	if di.PciBusNumber == "" {
		return nil, fmt.Errorf("missing PciBusNumber")
	}

	name := canonicalNameFromPCI(di.PciBusNumber)
	renderCardNames, err := getCardAndRender(di.PciBusNumber)
	if err != nil {
		// Don't fail hard: some envs might not have /sys/module mounted during dev.
		klog.Warningf("getCardAndRender failed for pci=%s: %v", di.PciBusNumber, err)
		renderCardNames = nil
	}

	return &AllocatableDevice{
		Name:            name,
		DvInd:           di.DvInd,
		PciBusNumber:    di.PciBusNumber,
		UUID:            di.DeviceId,
		ProductName:     di.DevTypeName,
		Architecture:    "",
		Brand:           di.SubsystemTypeName,
		ComputeUnits:    int64(math.Round(di.ComputeUnit)),
		MemoryBytes:     int64(math.Round(di.MemoryTotal)),
		RenderCardNames: renderCardNames,
	}, nil
}

func canonicalNameFromPCI(pci string) string {
	// ResourceSlice / DRA device name (PCI-based); CDI device name uses UUID separately.
	s := strings.NewReplacer(":", "-", ".", "-", " ", "").Replace(pci)
	return "hcu-" + s
}

// sanitizeCDIDeviceName makes a string safe for CDI spec device "name" fields.
func sanitizeCDIDeviceName(s string) string {
	return strings.NewReplacer(":", "-", " ", "_", "/", "-").Replace(strings.TrimSpace(s))
}

// physicalCDIDeviceName is the CDI devices[].name and CDI device ID suffix (e.g. HCU-TPXS300002100601).
func physicalCDIDeviceName(alloc *AllocatableDevice) string {
	if alloc.UUID != "" {
		return "HCU-" + sanitizeCDIDeviceName(alloc.UUID)
	}
	return alloc.Name
}

// vdeviceCDIName is the CDI name for a virtual device (e.g. vdev0).
func vdeviceCDIName(vdvInd int) string {
	return fmt.Sprintf("vdev%d", vdvInd)
}

func (d *AllocatableDevice) GetDevice() resourceapi.Device {
	// Consumable capacity: requires DRAConsumableCapacity on apiserver, scheduler, kubelet.
	// If any component lacks the gate, apiserver strips requestPolicy / allowMultipleAllocations
	// and kubeletplugin logs: "some fields were dropped ... DRAConsumableCapacity".
	coresQty := resource.NewQuantity(d.ComputeUnits, resource.DecimalSI)
	memQty := resource.NewQuantity(d.MemoryBytes, resource.BinarySI)

	min0Dec := resource.NewQuantity(0, resource.DecimalSI)
	min0Bin := resource.NewQuantity(0, resource.BinarySI)
	stepCore := resource.NewQuantity(1, resource.DecimalSI)
	stepMem := resource.NewQuantity(1024*1024, resource.BinarySI)

	allowed := true
	device := resourceapi.Device{
		Name: d.Name,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			attributeTypeKey: {
				StringValue: ptr.To(string(hcuDeviceType)),
			},
			attributeUUIDKey: {
				StringValue: ptr.To(d.UUID),
			},
			attributeProductNameKey: {
				StringValue: ptr.To(d.ProductName),
			},
			attributeArchitectureKey: {
				StringValue: ptr.To(d.Architecture),
			},
			attributeBrandKey: {
				StringValue: ptr.To(d.Brand),
			},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			capacityCoresKey: {
				Value: *coresQty,
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: coresQty,
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  min0Dec,
						Max:  coresQty,
						Step: stepCore,
					},
				},
			},
			capacityMemoryKey: {
				Value: *memQty,
				RequestPolicy: &resourceapi.CapacityRequestPolicy{
					Default: memQty,
					ValidRange: &resourceapi.CapacityRequestPolicyRange{
						Min:  min0Bin,
						Max:  memQty,
						Step: stepMem,
					},
				},
			},
		},
		AllowMultipleAllocations: &allowed,
	}
	return device
}

// DeviceInfo mirrors the subset we need from hcu-dcgm/pkg/dcgm.
// Keeping our own struct avoids importing dcgm types directly into this file.
type DeviceInfo struct {
	DvInd             int
	PciBusNumber      string
	DeviceId          string
	DevTypeName       string
	SubsystemTypeName string
	ComputeUnit       float64
	MemoryTotal       float64
}
