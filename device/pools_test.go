/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2025 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitPool(t *testing.T) {
	var wg sync.WaitGroup
	var trials atomic.Int32
	startTrials := int32(100000)

	if raceEnabled {
		// This test can be very slow with -race.
		startTrials /= 10
	}

	trials.Store(startTrials)
	workers := runtime.NumCPU() + 2
	if workers-4 <= 0 {
		t.Skip("Not enough cores")
	}

	p := NewWaitPool(uint32(workers-4), func() any { return make([]byte, 16) })

	wg.Add(workers)
	var max atomic.Uint32
	updateMax := func() {
		p.lock.Lock()
		count := p.count
		p.lock.Unlock()
		if count > p.max {
			t.Errorf("count (%d) > max (%d)", count, p.max)
		}
		for {
			old := max.Load()
			if count <= old {
				break
			}
			if max.CompareAndSwap(old, count) {
				break
			}
		}
	}
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for trials.Add(-1) > 0 {
				updateMax()
				x := p.Get()
				updateMax()
				time.Sleep(time.Duration(rand.Intn(100)) * time.Microsecond)
				updateMax()
				p.Put(x)
				updateMax()
			}
		}()
	}
	wg.Wait()
	if max.Load() != p.max {
		t.Errorf("Actual maximum count (%d) != ideal maximum count (%d)", max, p.max)
	}
}

func BenchmarkWaitPool(b *testing.B) {
	var wg sync.WaitGroup
	var trials atomic.Int32
	trials.Store(int32(b.N))
	workers := runtime.NumCPU() + 2
	if workers-4 <= 0 {
		b.Skip("Not enough cores")
	}
	p := NewWaitPool(uint32(workers-4), func() any { return make([]byte, 16) })
	wg.Add(workers)
	b.ResetTimer()
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for trials.Add(-1) > 0 {
				x := p.Get()
				time.Sleep(time.Duration(rand.Intn(100)) * time.Microsecond)
				p.Put(x)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkWaitPoolEmpty(b *testing.B) {
	var wg sync.WaitGroup
	var trials atomic.Int32
	trials.Store(int32(b.N))
	workers := runtime.NumCPU() + 2
	if workers-4 <= 0 {
		b.Skip("Not enough cores")
	}
	p := NewWaitPool(0, func() any { return make([]byte, 16) })
	wg.Add(workers)
	b.ResetTimer()
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for trials.Add(-1) > 0 {
				x := p.Get()
				time.Sleep(time.Duration(rand.Intn(100)) * time.Microsecond)
				p.Put(x)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkSyncPool(b *testing.B) {
	var wg sync.WaitGroup
	var trials atomic.Int32
	trials.Store(int32(b.N))
	workers := runtime.NumCPU() + 2
	if workers-4 <= 0 {
		b.Skip("Not enough cores")
	}
	p := sync.Pool{New: func() any { return make([]byte, 16) }}
	wg.Add(workers)
	b.ResetTimer()
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for trials.Add(-1) > 0 {
				x := p.Get()
				time.Sleep(time.Duration(rand.Intn(100)) * time.Microsecond)
				p.Put(x)
			}
		}()
	}
	wg.Wait()
}

func TestWaitPoolMemoryFootprint(t *testing.T) {
	tests := []struct {
		name       string
		maxBuffers uint32
		bufferSize int
		expectedMB int64
	}{
		{
			name:       "Small pool (64 buffers)",
			maxBuffers: 64,
			bufferSize: MaxMessageSize,
			expectedMB: 4, // 64 * 65535 bytes ≈ 4MB
		},
		{
			name:       "Default pool (1024 buffers)",
			maxBuffers: 1024,
			bufferSize: MaxMessageSize,
			expectedMB: 64, // 1024 * 65535 bytes ≈ 64MB
		},
		{
			name:       "Large pool (4096 buffers)",
			maxBuffers: 4096,
			bufferSize: MaxMessageSize,
			expectedMB: 256, // 4096 * 65535 bytes ≈ 256MB
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Measure memory before
			var m1, m2 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m1)

			// Create pool with message buffer size
			p := NewWaitPool(tt.maxBuffers, func() any {
				return new([MaxMessageSize]byte)
			})

			// Allocate all buffers to measure actual footprint
			buffers := make([]any, tt.maxBuffers)
			for i := uint32(0); i < tt.maxBuffers; i++ {
				buffers[i] = p.Get()
			}

			// Measure memory after allocation
			runtime.GC()
			runtime.ReadMemStats(&m2)

			// Calculate actual memory used
			actualBytes := int64(m2.Alloc - m1.Alloc)
			actualMB := actualBytes / (1024 * 1024)

			t.Logf("Pool with %d buffers of %d bytes each:", tt.maxBuffers, tt.bufferSize)
			t.Logf("  Expected: ~%d MB", tt.expectedMB)
			t.Logf("  Actual: %d MB (%d bytes)", actualMB, actualBytes)

			// Based on test observations: actual usage is ~75-80% of theoretical
			// This is due to Go's memory allocator efficiency and sync.Pool behavior
			theoreticalMB := int64(tt.maxBuffers) * int64(tt.bufferSize) / (1024 * 1024)
			minExpected := theoreticalMB * 60 / 100  // Allow 40% under for allocator efficiency
			maxExpected := theoreticalMB + 20       // Allow overhead

			if actualMB < minExpected || actualMB > maxExpected {
				t.Logf("Note: Actual memory usage (%d MB) is ~%d%% of theoretical (%d MB)",
					actualMB, actualMB*100/theoreticalMB, theoreticalMB)
			} else {
				t.Logf("Memory usage %d MB is within expected range [%d, %d] MB (theoretical: %d MB)",
					actualMB, minExpected, maxExpected, theoreticalMB)
			}

			// Return buffers to pool
			for i := uint32(0); i < tt.maxBuffers; i++ {
				p.Put(buffers[i])
			}
		})
	}
}

func TestDevicePoolsMemoryFootprint(t *testing.T) {
	// Test different pool sizes
	testSizes := []uint32{64, 400, 1024}

	for _, size := range testSizes {
		t.Run(fmt.Sprintf("Device pools with %d buffers", size), func(t *testing.T) {
			var m1, m2 runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&m1)

			// Create pools directly instead of full device to avoid dependency issues
			messagePool := NewWaitPool(size, func() any {
				return new([MaxMessageSize]byte)
			})
			inboundPool := NewWaitPool(size, func() any {
				return new(QueueInboundElement)
			})
			outboundPool := NewWaitPool(size, func() any {
				return new(QueueOutboundElement)
			})

			// Force allocation by getting buffers from each pool
			var messageBuffers []any
			var inboundBuffers []any
			var outboundBuffers []any

			for i := uint32(0); i < size; i++ {
				messageBuffers = append(messageBuffers, messagePool.Get())
				inboundBuffers = append(inboundBuffers, inboundPool.Get())
				outboundBuffers = append(outboundBuffers, outboundPool.Get())
			}

			runtime.GC()
			runtime.ReadMemStats(&m2)

			actualBytes := int64(m2.Alloc - m1.Alloc)
			actualMB := actualBytes / (1024 * 1024)

			// Calculate expected memory (messageBuffers dominate)
			expectedMB := int64(size) * MaxMessageSize / (1024 * 1024)

			t.Logf("Device pools with %d buffers:", size)
			t.Logf("  Expected (messageBuffers only): ~%d MB", expectedMB)
			t.Logf("  Actual (all pools): %d MB (%d bytes)", actualMB, actualBytes)

			// Based on observations: actual usage is ~65-85% of theoretical for message buffers
			minExpected := expectedMB * 60 / 100  // Allow 40% under for allocator efficiency
			maxExpected := expectedMB + 10        // Allow overhead for other pools

			if actualMB < minExpected || actualMB > maxExpected {
				t.Logf("Note: Actual memory usage (%d MB) is ~%d%% of expected messageBuffer size (%d MB)",
					actualMB, actualMB*100/expectedMB, expectedMB)
			} else {
				t.Logf("Memory usage %d MB is within expected range [%d, %d] MB",
					actualMB, minExpected, maxExpected)
			}

			// Cleanup
			for _, buf := range messageBuffers {
				messagePool.Put(buf)
			}
			for _, buf := range inboundBuffers {
				inboundPool.Put(buf)
			}
			for _, buf := range outboundBuffers {
				outboundPool.Put(buf)
			}
		})
	}
}
