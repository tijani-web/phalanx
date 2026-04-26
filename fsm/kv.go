// Package fsm implements the user-facing state machine applied by Raft.
//
// When the Node detects committed log entries, it unmarshals the Data
// field into a Command and calls KV.Apply(). This is the "output" of
// consensus — the replicated, deterministic computation.
package fsm

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Op defines the supported mutation operations.
const (
	OpSet    = "SET"
	OpDelete = "DELETE"
)

// Command is the unit of mutation serialized into LogEntry.Data.
type Command struct {
	Op    string `json:"op"`              // OpSet or OpDelete.
	Key   string `json:"key"`
	Value string `json:"value,omitempty"` // Only used for SET.
}

// Encode serializes a Command to JSON bytes for embedding in LogEntry.Data.
func (c Command) Encode() []byte {
	data, _ := json.Marshal(c)
	return data
}

// DecodeCommand deserializes a Command from LogEntry.Data.
func DecodeCommand(data []byte) (Command, error) {
	var cmd Command
	if err := json.Unmarshal(data, &cmd); err != nil {
		return cmd, fmt.Errorf("fsm: decode command: %w", err)
	}
	return cmd, nil
}

// KV is the replicated key-value state machine.
// All mutations flow through Apply() which is called only from
// the Node's single-threaded event loop, so the write path
// does not need locking. Reads use RLock for concurrent safety
// since clients may read while the loop applies.
type KV struct {
	mu   sync.RWMutex
	data map[string]string
}

// NewKV creates an empty key-value state machine.
func NewKV() *KV {
	return &KV{data: make(map[string]string)}
}

// Apply deserializes and applies a command to the KV store.
// Returns an error only for malformed data — a successful
// DELETE on a missing key is not an error.
func (kv *KV) Apply(data []byte) error {
	if len(data) == 0 {
		return nil // No-op entries (e.g., §8 leader noop).
	}
	cmd, err := DecodeCommand(data)
	if err != nil {
		return err
	}

	kv.mu.Lock()
	defer kv.mu.Unlock()

	switch cmd.Op {
	case OpSet:
		kv.data[cmd.Key] = cmd.Value
	case OpDelete:
		delete(kv.data, cmd.Key)
	default:
		return fmt.Errorf("fsm: unknown op %q", cmd.Op)
	}
	return nil
}

// Get reads a value from the KV store. Thread-safe for concurrent reads.
func (kv *KV) Get(key string) (value string, found bool) {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	value, found = kv.data[key]
	return
}

// Snapshot returns a point-in-time copy of the entire KV state.
// Used by the debug UI and snapshot support.
func (kv *KV) Snapshot() map[string]string {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	snap := make(map[string]string, len(kv.data))
	for k, v := range kv.data {
		snap[k] = v
	}
	return snap
}

// Len returns the number of keys in the store.
func (kv *KV) Len() int {
	kv.mu.RLock()
	defer kv.mu.RUnlock()
	return len(kv.data)
}
