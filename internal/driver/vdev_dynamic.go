// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
)

const (
	vdevConfDir        = "/etc/vdev"
	vdevDynamicDir     = vdevConfDir + "/dynamic"
	vdevDynamicDirPerm = 0o777
)

// dynamicMarkFileName matches k8s-hcu-device-plugin HAMI CreateMarkFile:
// {podUID}_{containerName}_{dvInd}_{vdevIdx} with vdevIdx=-1 for whole-card.
func dynamicMarkFileName(podUID, containerName string, dvInd, vdevIdx int) string {
	return fmt.Sprintf("%s_%s_%d_%d", podUID, containerName, dvInd, vdevIdx)
}

func ensureVDevDynamicDir() error {
	if err := os.MkdirAll(vdevDynamicDir, vdevDynamicDirPerm); err != nil {
		return fmt.Errorf("mkdir %s: %w", vdevDynamicDir, err)
	}
	return os.Chmod(vdevDynamicDir, vdevDynamicDirPerm)
}

func createDynamicMarkFile(podUID, containerName string, dvInd, vdevIdx int) (string, error) {
	if err := ensureVDevDynamicDir(); err != nil {
		return "", err
	}
	name := dynamicMarkFileName(podUID, containerName, dvInd, vdevIdx)
	path := filepath.Join(vdevDynamicDir, name)
	if err := os.WriteFile(path, []byte(time.Now().Format(time.DateTime)), vdevDynamicDirPerm); err != nil {
		return "", fmt.Errorf("write dynamic mark %q: %w", path, err)
	}
	klog.Infof("Created vdev dynamic mark: %s", path)
	return path, nil
}

func removeDynamicMarkFile(basename string) error {
	if basename == "" {
		return nil
	}
	path := filepath.Join(vdevDynamicDir, basename)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove dynamic mark %q: %w", path, err)
	}
	if err == nil {
		klog.Infof("Removed vdev dynamic mark: %s", path)
	}
	return nil
}

// removeDynamicMarksByClaimUID removes fallback marks created when Pod UID was unknown
// ({claimUID}_dra_*). Helps Unprepare after Prepare partially succeeded or claim was
// deleted before kubelet called Unprepare.
func removeDynamicMarksByClaimUID(claimUID string) {
	if claimUID == "" {
		return
	}
	prefix := claimUID + "_"
	entries, err := os.ReadDir(vdevDynamicDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		klog.Warningf("remove dynamic marks by claim UID: read %s: %v", vdevDynamicDir, err)
		return
	}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasPrefix(ent.Name(), prefix) {
			continue
		}
		if err := removeDynamicMarkFile(ent.Name()); err != nil {
			klog.Warningf("remove dynamic marks by claim UID=%s: %v", claimUID, err)
		}
	}
}

func dynamicMarkVDevIndex(wholeCard bool, vdvInd int) int {
	if wholeCard {
		return -1
	}
	return vdvInd
}

// syncDynamicMarkFiles ensures mark files exist for prepared claims and deletes stale entries.
// Caller must hold s.mu.
func (s *DeviceState) syncDynamicMarkFiles(reason string) error {
	if err := ensureVDevDynamicDir(); err != nil {
		return err
	}

	active := make(map[string]struct{})
	for _, list := range s.prepared {
		for _, p := range list {
			if p.DynamicMarkFile == "" {
				continue
			}
			active[p.DynamicMarkFile] = struct{}{}
			markPath := filepath.Join(vdevDynamicDir, p.DynamicMarkFile)
			if _, err := os.Stat(markPath); err != nil {
				podUID := p.PodUID
				containerName := p.ContainerName
				if podUID == "" {
					podUID = string(p.ClaimUID)
					containerName = "dra"
				}
				if _, err := createDynamicMarkFile(podUID, containerName, p.DvInd, dynamicMarkVDevIndex(p.WholeCard, p.VDvInd)); err != nil {
					return err
				}
				klog.Infof("Recreated vdev dynamic mark %s (reason=%s)", p.DynamicMarkFile, reason)
			}
		}
	}

	entries, err := os.ReadDir(vdevDynamicDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", vdevDynamicDir, err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if _, ok := active[ent.Name()]; ok {
			continue
		}
		stalePath := filepath.Join(vdevDynamicDir, ent.Name())
		if err := os.Remove(stalePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale dynamic mark %q: %w", stalePath, err)
		}
		klog.Infof("Removed stale vdev dynamic mark: %s (reason=%s)", stalePath, reason)
	}
	return nil
}

func removePreparedDynamicMarks(prepared []PreparedDevice) {
	for _, p := range prepared {
		if err := removeDynamicMarkFile(p.DynamicMarkFile); err != nil {
			klog.Warningf("remove dynamic mark claim=%s file=%s: %v", p.ClaimUID, p.DynamicMarkFile, err)
		}
	}
}

// attachDynamicMark fills PreparedDevice dynamic-mark fields and creates the host file.
func (s *DeviceState) attachDynamicMark(ctx context.Context, claim *resourceapi.ResourceClaim, p *PreparedDevice, alloc *AllocatableDevice) error {
	podUID, containerName, err := resolveClaimConsumer(ctx, s.clientset, s.nodeName, claim)
	if err != nil {
		klog.Warningf("resolve claim consumer for dynamic mark claim=%s: %v; using claim UID fallback", claim.UID, err)
		podUID = string(claim.UID)
		containerName = "dra"
	}
	p.PodUID = podUID
	p.ContainerName = containerName
	p.DvInd = alloc.DvInd

	vdevIdx := dynamicMarkVDevIndex(p.WholeCard, p.VDvInd)
	markPath, err := createDynamicMarkFile(podUID, containerName, alloc.DvInd, vdevIdx)
	if err != nil {
		return err
	}
	p.DynamicMarkFile = filepath.Base(markPath)
	return nil
}
