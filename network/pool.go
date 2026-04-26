package network

import (
	"github.com/basit-tijani/phalanx/pb"
	"sync"
)

// entryPool is the global pool for LogEntry messages.
// All hot-path code must lease entries from this pool to minimize
// heap allocations and GC pressure.
//
// Usage pattern:
//
//	entry := AcquireEntry()
//	entry.Index = 42
//	entry.Term  = 3
//	entry.Data  = append(entry.Data, payload...)
//	// ... use entry ...
//	ReleaseEntry(entry) // caller must not reference entry after this
var entryPool = sync.Pool{
	New: func() any {
		return &pb.LogEntry{
			Data: make([]byte, 0, 256), // pre-allocate reasonable capacity
		}
	},
}

// AcquireEntry leases a zeroed LogEntry from the pool.
// The returned entry has all fields zeroed but retains
// the Data slice's underlying capacity from prior use.
func AcquireEntry() *pb.LogEntry {
	return entryPool.Get().(*pb.LogEntry)
}

// ReleaseEntry returns a LogEntry to the pool after resetting it.
// The caller MUST NOT reference the entry after this call.
// Passing nil is a safe no-op.
func ReleaseEntry(e *pb.LogEntry) {
	if e == nil {
		return
	}
	e.Reset()
	entryPool.Put(e)
}
