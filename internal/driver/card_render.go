// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// pciPathCandidates returns sysfs /sys/bus/pci/devices/<id> style IDs to try.
func pciPathCandidates(pcieAddress string) []string {
	pci := strings.TrimPrefix(strings.TrimSpace(pcieAddress), "pci:")
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, x := range out {
			if x == s {
				return
			}
		}
		out = append(out, s)
	}
	add(pci)
	// Short BDF like 00:08.0 → also try with domain 0000:
	if strings.Count(pci, ":") == 1 {
		add("0000:" + pci)
	}
	// Long form already present; also try suffix after first colon (module path style)
	if i := strings.Index(pcieAddress, ":"); i >= 0 {
		rest := strings.TrimSpace(pcieAddress[i+1:])
		if rest != "" {
			add(rest)
		}
	}
	return out
}

func isDRIDirName(name string) bool {
	return strings.HasPrefix(name, "card") || strings.HasPrefix(name, "renderD")
}

// getCardAndRenderFromSysBus lists drm nodes under /sys/bus/pci/devices/<pci>/drm
// (works when the driver is not exposed under /sys/module/*/drivers/pci:*).
func getCardAndRenderFromSysBus(pcieAddress string) ([]string, error) {
	var lastErr error
	for _, pci := range pciPathCandidates(pcieAddress) {
		drmPath := filepath.Join("/sys/bus/pci/devices", pci, "drm")
		entries, err := os.ReadDir(drmPath)
		if err != nil {
			lastErr = err
			continue
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() && isDRIDirName(e.Name()) {
				names = append(names, e.Name())
			}
		}
		if len(names) == 0 {
			lastErr = fmt.Errorf("no card/render entries under %s", drmPath)
			continue
		}
		sort.Strings(names)
		return names, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("sysfs pci drm for %q: %w", pcieAddress, lastErr)
	}
	return nil, fmt.Errorf("sysfs pci drm for %q: no candidates", pcieAddress)
}

// discoverDRINodes tries sysfs PCI bus first, then legacy /sys/module/.../drivers paths.
func discoverDRINodes(pcieAddress string) ([]string, error) {
	if n, err := getCardAndRenderFromSysBus(pcieAddress); err == nil && len(n) > 0 {
		return n, nil
	}
	return getCardAndRender(pcieAddress)
}

// getCardAndRender returns the drm entries (e.g. renderD128, card0) that
// correspond to the given PCI address.
func getCardAndRender(pcieAddress string) ([]string, error) {
	modules := []string{"hycu"}

	// Try a few PCI string shapes; sysfs may use pci:0000:03:00.0 or pci:03:00.0.
	candidates := []string{pcieAddress}
	if i := strings.Index(pcieAddress, ":"); i >= 0 {
		rest := pcieAddress[i+1:]
		if rest != "" && !sliceContainsString(candidates, rest) {
			candidates = append(candidates, rest)
		}
	}

	for _, module := range modules {
		for _, pci := range candidates {
			// Expected shape:
			// /sys/module/<module>/drivers/pci:<pcieAddress>/drm/<entry>
			dirPath := filepath.Join("/sys/module", module, "drivers/pci:"+pci, "drm")
			if _, err := os.Stat(dirPath); err != nil {
				continue
			}

			dir, err := os.Open(dirPath)
			if err != nil {
				return nil, fmt.Errorf("open %q: %w", dirPath, err)
			}
			subDirs, rerr := dir.Readdirnames(-1)
			_ = dir.Close()
			if rerr != nil {
				return nil, fmt.Errorf("readdirnames %q: %w", dirPath, rerr)
			}
			return subDirs, nil
		}
	}

	return nil, fmt.Errorf("no matching drm dir under /sys for pci=%s (checked %v)", pcieAddress, modules)
}

func sliceContainsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
