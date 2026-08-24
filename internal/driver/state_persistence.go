// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

const preparedStateFileName = "hcu-dra-driver-prepared-state.state"

// persistedState is the on-disk representation of prepared devices.
type persistedState struct {
	Prepared map[types.UID][]PreparedDevice `json:"prepared"`
}

// savePreparedState writes the current prepared map to disk.
// Caller must hold s.mu.
func (s *DeviceState) savePreparedState() error {
	state := persistedState{Prepared: s.prepared}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal prepared state: %w", err)
	}

	// Write to a temp file first and rename for atomicity.
	tmpPath := s.stateFile + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write prepared state tmp: %w", err)
	}
	if err := os.Rename(tmpPath, s.stateFile); err != nil {
		return fmt.Errorf("rename prepared state: %w", err)
	}
	return nil
}

// loadPreparedState restores the prepared map from disk.
// Returns an empty map if the file does not exist.
func loadPreparedState(stateFile string) (map[types.UID][]PreparedDevice, error) {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[types.UID][]PreparedDevice), nil
		}
		return nil, fmt.Errorf("read prepared state: %w", err)
	}
	if len(data) == 0 {
		return make(map[types.UID][]PreparedDevice), nil
	}

	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		klog.Warningf("corrupt prepared state file %s, starting fresh: %v", stateFile, err)
		return make(map[types.UID][]PreparedDevice), nil
	}
	if state.Prepared == nil {
		state.Prepared = make(map[types.UID][]PreparedDevice)
	}
	return state.Prepared, nil
}

// stateFilePath returns the full path to the prepared state file.
func stateFilePath(cdiRoot string) string {
	return filepath.Join(cdiRoot, preparedStateFileName)
}
