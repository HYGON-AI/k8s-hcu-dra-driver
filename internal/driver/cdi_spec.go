// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

const (
	cdiDeviceClass  = "hcu"
	cdiVDeviceClass = "vhcu"
	// Match legacy hygon HCU CDI specs (containerd accepts 0.5.x and 0.6.x).
	cdiVersion = "0.5.0"

	cdiHookPath = "/usr/bin/hcu-cdi-hook"

	cdiSpecFileSuffix = ".yaml"

	cdiVDeviceSpecPrefix = "hcu-dra-driver-vdevice-"

	cdiDeviceSpecFileName         = "hcu-dra-driver-device" + cdiSpecFileSuffix
	cdiVDeviceLegacyAggregateName = "hcu-dra-driver-vdevice.json"
	cdiLegacyJSONSuffix           = ".json"
)

type cdiDeviceNode struct {
	Path string `json:"path"`
}

type cdiHook struct {
	HookName string   `json:"hookName"`
	Path     string   `json:"path"`
	Args     []string `json:"args,omitempty"`
}

type cdiMountSpec struct {
	HostPath      string   `json:"hostPath"`
	ContainerPath string   `json:"containerPath"`
	Options       []string `json:"options,omitempty"`
}

type cdiContainerEdits struct {
	DeviceNodes []cdiDeviceNode `json:"deviceNodes,omitempty"`
	Env         []string        `json:"env,omitempty"`
	Mounts      []cdiMountSpec  `json:"mounts,omitempty"`
	Hooks       []cdiHook       `json:"hooks,omitempty"`
}

type cdiDeviceEntry struct {
	Name           string            `json:"name"`
	ContainerEdits cdiContainerEdits `json:"containerEdits,omitempty"`
}

type cdiSpec struct {
	CDIVersion string `json:"cdiVersion" yaml:"cdiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	// Pointer so an unset root is omitted from YAML (avoid "containerEdits: {}" confusing runtimes).
	ContainerEdits *cdiContainerEdits `json:"containerEdits,omitempty" yaml:"containerEdits,omitempty"`
	Devices        []cdiDeviceEntry   `json:"devices" yaml:"devices"`
}

var (
	cdiBindROMountOpts = []string{"ro", "nosuid", "nodev", "bind"}
	cdiRBindMountOpts  = []string{"rbind", "rprivate"}
)

func normalizePCIAddr(pci string) string {
	return strings.TrimPrefix(strings.TrimSpace(pci), "pci:")
}

func pciDRByPathPrefix(pci string) string {
	return "pci-" + normalizePCIAddr(pci)
}

// pciSysfsDevicePath resolves the real /sys/devices/... path for a PCI BDF.
// Devices behind PCI bridges are nested (e.g. pci0000:70/.../0000:77:00.0), so
// string concatenation like /sys/devices/pci<domain>:<bus>/<bdf> is wrong.
// /sys/bus/pci/devices/<bdf> is always a symlink to the canonical topology path.
func pciSysfsDevicePath(pci string) (string, error) {
	var lastErr error
	for _, cand := range pciPathCandidates(pci) {
		link := filepath.Join("/sys/bus/pci/devices", cand)
		resolved, err := filepath.EvalSymlinks(link)
		if err != nil {
			lastErr = err
			continue
		}
		if _, err := os.Stat(resolved); err != nil {
			lastErr = err
			continue
		}
		return resolved, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("resolve sysfs path for pci %q: %w", pci, lastErr)
	}
	return "", fmt.Errorf("resolve sysfs path for pci %q: no candidates", pci)
}

func splitDRINames(driNames []string) (cardName, renderName string) {
	for _, n := range driNames {
		switch {
		case strings.HasPrefix(n, "card"):
			cardName = n
		case strings.HasPrefix(n, "render"):
			renderName = n
		}
	}
	return cardName, renderName
}

func commonGlobalContainerEdits() cdiContainerEdits {
	edits := cdiContainerEdits{
		DeviceNodes: []cdiDeviceNode{
			{Path: "/dev/kfd"},
			{Path: "/dev/mem"},
		},
		Env: []string{"HCU_VISIBLE_DEVICES=void"},
		Mounts: []cdiMountSpec{
			{
				HostPath:      "/opt/hyhal",
				ContainerPath: "/opt/hyhal",
				Options:       cdiBindROMountOpts,
			},
			{
				HostPath:      "/usr/local/hyhal",
				ContainerPath: "/usr/local/hyhal",
				Options:       cdiBindROMountOpts,
			},
			{
				HostPath:      "/sys/kernel/debug",
				ContainerPath: "/sys/kernel/debug",
				Options:       cdiBindROMountOpts,
			},
		},
	}
	if _, err := os.Stat("/dev/mkfd"); err == nil {
		edits.DeviceNodes = append(edits.DeviceNodes, cdiDeviceNode{Path: "/dev/mkfd"})
	}
	return edits
}

func driSymlinkHooks(pci string, driNames []string) []cdiHook {
	cardName, renderName := splitDRINames(driNames)
	if cardName == "" && renderName == "" {
		return nil
	}
	byPath := pciDRByPathPrefix(pci)
	linkArgs := []string{"hcu-cdi-hook", "create-symlinks"}
	if cardName != "" {
		linkArgs = append(linkArgs, "--link", fmt.Sprintf("../%s::/dev/dri/by-path/%s-card", cardName, byPath))
	}
	if renderName != "" {
		linkArgs = append(linkArgs, "--link", fmt.Sprintf("../%s::/dev/dri/by-path/%s-render", renderName, byPath))
	}
	return []cdiHook{
		{
			HookName: "createContainer",
			Path:     cdiHookPath,
			Args:     linkArgs,
		},
		{
			HookName: "createContainer",
			Path:     cdiHookPath,
			Args:     []string{"hcu-cdi-hook", "chmod", "--mode", "755", "--path", "/dev/dri"},
		},
	}
}

func mergeContainerEdits(global, per cdiContainerEdits) cdiContainerEdits {
	out := cdiContainerEdits{}
	out.DeviceNodes = append(append([]cdiDeviceNode{}, global.DeviceNodes...), per.DeviceNodes...)
	out.Env = append(append([]string{}, global.Env...), per.Env...)
	out.Mounts = append(append([]cdiMountSpec{}, global.Mounts...), per.Mounts...)
	out.Hooks = append(append([]cdiHook{}, global.Hooks...), per.Hooks...)
	return out
}

func perCardContainerEdits(pci string, driNames []string, vdevConfStem string) (cdiContainerEdits, error) {
	edits := cdiContainerEdits{}

	for _, name := range driNames {
		edits.DeviceNodes = append(edits.DeviceNodes, cdiDeviceNode{Path: "/dev/dri/" + name})
	}

	if hooks := driSymlinkHooks(pci, driNames); len(hooks) > 0 {
		edits.Hooks = hooks
	}

	if sysfsPath, err := pciSysfsDevicePath(pci); err == nil {
		edits.Mounts = append(edits.Mounts, cdiMountSpec{
			HostPath:      sysfsPath,
			ContainerPath: sysfsPath,
			Options:       cdiRBindMountOpts,
		})
	} else {
		klog.Warningf("skip sysfs mount for pci=%s: %v", pci, err)
	}

	if vdevConfStem != "" {
		edits.Mounts = append(edits.Mounts, cdiMountSpec{
			HostPath:      "/etc/vdev/" + vdevConfStem + ".conf",
			ContainerPath: "/etc/vdev/docker/" + vdevConfStem + ".conf",
			Options:       cdiBindROMountOpts,
		})
	}

	// When sysfs discovery failed, many runtimes still need /dev/dri visible (no per-node hooks).
	if len(driNames) == 0 {
		edits.Mounts = append(edits.Mounts, cdiMountSpec{
			HostPath:      "/dev/dri",
			ContainerPath: "/dev/dri",
			Options:       cdiRBindMountOpts,
		})
		klog.Warningf("CDI pci=%s: no card/render nodes discovered; using /dev/dri bind fallback", pci)
	}

	return edits, nil
}

func buildPhysicalDeviceEntry(alloc *AllocatableDevice) (cdiDeviceEntry, error) {
	driNames := alloc.RenderCardNames
	if len(driNames) == 0 {
		var err error
		driNames, err = discoverDRINodes(alloc.PciBusNumber)
		if err != nil {
			klog.Warningf("discover DRINodes for CDI pci=%s: %v", alloc.PciBusNumber, err)
		}
	}
	perDev, err := perCardContainerEdits(alloc.PciBusNumber, driNames, "")
	if err != nil {
		return cdiDeviceEntry{}, err
	}
	// Many CDI consumers only apply devices[].containerEdits (not spec-level edits).
	merged := mergeContainerEdits(commonGlobalContainerEdits(), perDev)
	return cdiDeviceEntry{
		Name:           physicalCDIDeviceName(alloc),
		ContainerEdits: merged,
	}, nil
}

// resolvePCIForVDeviceDRI picks a sysfs PCI address that can enumerate /dev/dri nodes.
// vHCU reports from dcgm may not match /sys/bus/pci/devices/*; parent ResourceSlice device PCI is authoritative.
func resolvePCIForVDeviceDRI(s *DeviceState, p PreparedDevice) (pci string, driNames []string) {
	var tryPCIs []string
	if alloc := s.allocatable[p.ParentDevice]; alloc != nil && alloc.PciBusNumber != "" {
		tryPCIs = append(tryPCIs, alloc.PciBusNumber)
	}
	if p.PciBusNumber != "" && !sliceContainsString(tryPCIs, p.PciBusNumber) {
		tryPCIs = append(tryPCIs, p.PciBusNumber)
	}
	for _, cand := range tryPCIs {
		names, err := discoverDRINodes(cand)
		if err != nil {
			klog.V(4).Infof("discover DRINodes vdev=%s pci=%s: %v", p.CDIName, cand, err)
			continue
		}
		if len(names) > 0 {
			return cand, names
		}
	}
	if len(tryPCIs) > 0 {
		return tryPCIs[0], nil
	}
	return p.PciBusNumber, nil
}

func buildVDeviceEntry(s *DeviceState, p PreparedDevice) (cdiDeviceEntry, error) {
	pciForDRI, driNames := resolvePCIForVDeviceDRI(s, p)
	if len(driNames) == 0 {
		klog.Warningf("vdevice %s: no DRI nodes for pci candidates (parent=%q vdevPci=%q); using /dev/dri fallback",
			p.CDIName, p.ParentDevice, p.PciBusNumber)
	}
	stem := fmt.Sprintf("vdev%d", p.VDvInd)
	perDev, err := perCardContainerEdits(pciForDRI, driNames, stem)
	if err != nil {
		return cdiDeviceEntry{}, err
	}
	merged := mergeContainerEdits(commonGlobalContainerEdits(), perDev)
	return cdiDeviceEntry{
		Name:           p.CDIName,
		ContainerEdits: merged,
	}, nil
}

func (s *DeviceState) cdiStandardDeviceID(alloc *AllocatableDevice) string {
	return fmt.Sprintf("%s/%s=%s", s.cdiVendor, cdiDeviceClass, physicalCDIDeviceName(alloc))
}

func (s *DeviceState) createStandardCDISpecFile() error {
	if err := os.MkdirAll(s.cdiRoot, 0755); err != nil {
		return fmt.Errorf("mkdir cdiRoot %q: %w", s.cdiRoot, err)
	}

	devices := make([]cdiDeviceEntry, 0, len(s.allocatable))
	for _, alloc := range s.allocatable {
		entry, err := buildPhysicalDeviceEntry(alloc)
		if err != nil {
			return err
		}
		devices = append(devices, entry)
	}

	// Per-device entries include merged global edits (kfd, hyhal, …) so runtimes that
	// only apply devices[].containerEdits still inject the full stack.
	spec := cdiSpec{
		CDIVersion: cdiVersion,
		Kind:       fmt.Sprintf("%s/%s", s.cdiVendor, cdiDeviceClass),
		Devices:    devices,
	}

	specPath := filepath.Join(s.cdiRoot, cdiDeviceSpecFileName)
	klog.Infof("Writing CDI base spec: %s (devices=%d)", specPath, len(devices))
	if err := writeCDISpecFile(specPath, spec); err != nil {
		return err
	}
	removeLegacyCDIJSONFiles(s.cdiRoot, "device-spec-refresh")
	return nil
}

func (s *DeviceState) cdiVDeviceID(name string) string {
	return fmt.Sprintf("%s/%s=%s", s.cdiVendor, cdiVDeviceClass, name)
}

func (s *DeviceState) cdiVDeviceKind() string {
	return fmt.Sprintf("%s/%s", s.cdiVendor, cdiVDeviceClass)
}

func (s *DeviceState) cdiVDevicePerFilePath(cdiName string) string {
	return filepath.Join(s.cdiRoot, cdiVDeviceSpecPrefix+cdiName+cdiSpecFileSuffix)
}

func (s *DeviceState) activePreparedVDevices() map[string]PreparedDevice {
	active := make(map[string]PreparedDevice)
	for _, list := range s.prepared {
		for _, p := range list {
			if p.WholeCard {
				continue
			}
			if prev, ok := active[p.CDIName]; ok && prev.ClaimUID != p.ClaimUID {
				klog.Warningf("duplicate CDI vdevice name %q: claim=%s overrides claim=%s (vdvInd=%d)",
					p.CDIName, p.ClaimUID, prev.ClaimUID, p.VDvInd)
			}
			active[p.CDIName] = p
		}
	}
	return active
}

func (s *DeviceState) vdeviceCDISpecFilesNeedRepair() bool {
	for _, p := range s.activePreparedVDevices() {
		if _, err := os.Stat(s.cdiVDevicePerFilePath(p.CDIName)); err != nil {
			return true
		}
	}
	return false
}

func writeCDISpecFile(path string, spec cdiSpec) error {
	out, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal cdi spec %q: %w", path, err)
	}
	// Leading document separator matches hand-written hygon CDI YAML files.
	content := append([]byte("---\n"), out...)
	return os.WriteFile(path, content, 0644)
}

func removeLegacyCDIJSONFiles(cdiRoot string, reason string) {
	for _, name := range []string{
		cdiDeviceSpecFileName[:len(cdiDeviceSpecFileName)-len(cdiSpecFileSuffix)] + cdiLegacyJSONSuffix,
		cdiVDeviceLegacyAggregateName,
	} {
		path := filepath.Join(cdiRoot, name)
		if err := os.Remove(path); err == nil {
			klog.Infof("Removed legacy CDI JSON spec: %s (reason=%s)", path, reason)
		}
	}
}

// syncVDeviceCDISpecFiles writes one CDI YAML per active vdevice and removes stale files.
// Caller must hold s.mu.
func (s *DeviceState) syncVDeviceCDISpecFiles(reason string) error {
	if err := os.MkdirAll(s.cdiRoot, 0755); err != nil {
		return fmt.Errorf("mkdir cdiRoot %q: %w", s.cdiRoot, err)
	}

	kind := s.cdiVDeviceKind()
	active := s.activePreparedVDevices()

	for cdiName, p := range active {
		entry, err := buildVDeviceEntry(s, p)
		if err != nil {
			return err
		}
		perPath := s.cdiVDevicePerFilePath(cdiName)
		perSpec := cdiSpec{
			CDIVersion: cdiVersion,
			Kind:       kind,
			Devices:    []cdiDeviceEntry{entry},
		}
		if err := writeCDISpecFile(perPath, perSpec); err != nil {
			return err
		}
		klog.Infof("Writing CDI vdevice spec: %s (device=%s reason=%s)", perPath, cdiName, reason)
	}

	entries, err := os.ReadDir(s.cdiRoot)
	if err != nil {
		return fmt.Errorf("read cdiRoot %q: %w", s.cdiRoot, err)
	}
	prefix := cdiVDeviceSpecPrefix
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, cdiLegacyJSONSuffix) {
			stalePath := filepath.Join(s.cdiRoot, name)
			_ = os.Remove(stalePath)
			klog.Infof("Removed legacy CDI vdevice JSON: %s (reason=%s)", stalePath, reason)
			continue
		}
		if !strings.HasSuffix(name, cdiSpecFileSuffix) {
			continue
		}
		cdiName := strings.TrimSuffix(strings.TrimPrefix(name, prefix), cdiSpecFileSuffix)
		if _, ok := active[cdiName]; ok {
			continue
		}
		stalePath := filepath.Join(s.cdiRoot, name)
		if err := os.Remove(stalePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale cdi spec %q: %w", stalePath, err)
		}
		klog.Infof("Removed stale CDI vdevice spec: %s (reason=%s)", stalePath, reason)
	}

	removeLegacyCDIJSONFiles(s.cdiRoot, reason)
	return nil
}
