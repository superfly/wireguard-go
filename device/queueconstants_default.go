//go:build !android && !ios && !windows

/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"os"
	"strconv"
	"golang.zx2c4.com/wireguard/conn"
)

const (
	QueueStagedSize    = conn.IdealBatchSize
	QueueOutboundSize  = 1024
	QueueInboundSize   = 1024
	QueueHandshakeSize = 1024
	MaxSegmentSize     = (1 << 16) - 1 // largest possible UDP datagram
)

var PreallocatedBuffersPerPool = getPreallocatedBuffersPerPool()

func getPreallocatedBuffersPerPool() uint32 {
	if env := os.Getenv("WIREGUARD_PREALLOC_BUFFERS"); env != "" {
		if val, err := strconv.ParseUint(env, 10, 32); err == nil {
			return uint32(val)
		}
	}
	return 1024 // default value
}
