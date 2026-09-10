// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"fmt"
	"os"
	"strconv"
)

// DriverName is the DRA driver name registered with kubelet and used in ResourceSlice.
const DriverName = "dra.hygon.com"

const (
	deviceSplitCountEnv     = "DEVICE_SPLIT_COUNT"
	defaultDeviceSplitCount = 4
)

// DeviceSplitCount is the max number of vHCUs one physical HCU can be split into.
// Loaded from DEVICE_SPLIT_COUNT at startup; defaults to 4.
var DeviceSplitCount int64 = defaultDeviceSplitCount

// InitDeviceSplitCountFromEnv loads DEVICE_SPLIT_COUNT into DeviceSplitCount.
// Empty value keeps the default; non-positive or non-integer values return an error.
func InitDeviceSplitCountFromEnv() error {
	s := os.Getenv(deviceSplitCountEnv)
	if s == "" {
		DeviceSplitCount = defaultDeviceSplitCount
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be a positive integer: %w", deviceSplitCountEnv, err)
	}
	if n <= 0 {
		return fmt.Errorf("%s must be a positive integer, got %d", deviceSplitCountEnv, n)
	}
	DeviceSplitCount = n
	return nil
}
