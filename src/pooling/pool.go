package pooling

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/logging"
	"sync"
)

// BufferPool manages reusable byte slices to reduce GC pressure
type BufferPool struct {
	pool sync.Pool
	size int
}

// NewBufferPool creates a new buffer pool with fixed-size buffers
func NewBufferPool(bufferSize int) *BufferPool {
	return &BufferPool{
		pool: sync.Pool{
			New: func() interface{} {
				return make([]byte, bufferSize)
			},
		},
		size: bufferSize,
	}
}

// Get retrieves a buffer from the pool
func (bp *BufferPool) Get() []byte {
	return bp.pool.Get().([]byte)
}

// Put returns a buffer to the pool
func (bp *BufferPool) Put(buf []byte) {
	// Only return buffers of the expected size to maintain pool consistency
	if len(buf) == bp.size {
		bp.pool.Put(buf)
	}
}

// Global buffer pools for different use cases
var (
	StatusResponsePool *BufferPool
	ReadBufferPool     *BufferPool
)

// InitBufferPools initializes the global buffer pools
func InitBufferPools(bufferSize int) {
	StatusResponsePool = NewBufferPool(constants.StatusResponseSize)
	ReadBufferPool = NewBufferPool(bufferSize)

	logging.Info.Printf("Initialized buffer pools - read: %d bytes, status: %d bytes",
		bufferSize, constants.StatusResponseSize)
}
