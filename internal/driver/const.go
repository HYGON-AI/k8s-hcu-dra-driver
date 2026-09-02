// Copyright (c) 2026 Hygon Information Technology Co., Ltd.
// SPDX-License-Identifier: Apache-2.0

package driver

// DriverName is the DRA driver name registered with kubelet and used in ResourceSlice.
const DriverName = "dra.hygon.com"

// MaxVDevicesPerHCU is the hardware limit: one physical HCU can be split into at most 4 vHCUs.
const MaxVDevicesPerHCU = 4
