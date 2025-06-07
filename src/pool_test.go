package main

import (
	"sync"
	"testing"
)

func TestBufferPool(t *testing.T) {
	bufferSize := 1024
	pool := NewBufferPool(bufferSize)

	// Test basic get/put operations
	buf1 := pool.Get()
	if len(buf1) != bufferSize {
		t.Errorf("Expected buffer size %d, got %d", bufferSize, len(buf1))
	}

	buf2 := pool.Get()
	if len(buf2) != bufferSize {
		t.Errorf("Expected buffer size %d, got %d", bufferSize, len(buf2))
	}

	// Buffers should be different instances
	if &buf1[0] == &buf2[0] {
		t.Error("Expected different buffer instances")
	}

	// Test putting buffers back
	pool.Put(buf1)
	pool.Put(buf2)

	// Test reuse
	buf3 := pool.Get()
	if len(buf3) != bufferSize {
		t.Errorf("Expected buffer size %d, got %d", bufferSize, len(buf3))
	}
}

func TestBufferPoolConcurrency(t *testing.T) {
	bufferSize := 512
	pool := NewBufferPool(bufferSize)
	numGoroutines := 100
	numOperations := 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				buf := pool.Get()
				if len(buf) != bufferSize {
					t.Errorf("Expected buffer size %d, got %d", bufferSize, len(buf))
				}

				// Simulate some work
				buf[0] = byte(j % 256)

				pool.Put(buf)
			}
		}()
	}

	wg.Wait()
}

func TestBufferPoolWrongSize(t *testing.T) {
	bufferSize := 1024
	pool := NewBufferPool(bufferSize)

	// Create a buffer of wrong size
	wrongSizeBuffer := make([]byte, bufferSize*2)

	// This should not panic and should not add the buffer to the pool
	pool.Put(wrongSizeBuffer)

	// Getting a buffer should still return the correct size
	buf := pool.Get()
	if len(buf) != bufferSize {
		t.Errorf("Expected buffer size %d, got %d", bufferSize, len(buf))
	}
}

func TestInitBufferPools(t *testing.T) {
	bufferSize := 2048

	// This should not panic
	InitBufferPools(bufferSize)

	// Test that global pools are initialized
	if statusResponsePool == nil {
		t.Error("statusResponsePool should be initialized")
	}

	if readBufferPool == nil {
		t.Error("readBufferPool should be initialized")
	}

	// Test that we can get buffers from global pools
	statusBuf := statusResponsePool.Get()
	if len(statusBuf) != StatusResponseSize {
		t.Errorf("Expected status buffer size %d, got %d", StatusResponseSize, len(statusBuf))
	}

	readBuf := readBufferPool.Get()
	if len(readBuf) != bufferSize {
		t.Errorf("Expected read buffer size %d, got %d", bufferSize, len(readBuf))
	}

	// Clean up
	statusResponsePool.Put(statusBuf)
	readBufferPool.Put(readBuf)
}
