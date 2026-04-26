// Package pb defines the core message types for Phalanx.
//
// These are hand-written Go structs that mirror proto/phalanx.proto.
// They will be replaced by generated code when gRPC transport is
// wired in Phase 3. Using native structs now keeps Phase 1
// dependency-free and avoids protoc toolchain requirements.
package pb

// EntryType classifies log entries.
type EntryType uint8

const (
	EntryCommand      EntryType = 0 // Client state-machine command.
	EntryConfigChange EntryType = 1 // Raft §6 membership configuration change.
)

// LogEntry is the fundamental unit of replicated state.
// All fields are value types to avoid pointer chasing on the hot path.
type LogEntry struct {
	Index uint64
	Term  uint64
	Data  []byte
	Type  EntryType
}

// Reset zeros the entry for pool recycling.
// Retains the underlying Data slice capacity to avoid reallocation.
func (e *LogEntry) Reset() {
	e.Index = 0
	e.Term = 0
	e.Data = e.Data[:0]
	e.Type = EntryCommand
}

// Clone returns a deep copy of the entry.
// The caller owns the returned entry — it is not pooled.
func (e *LogEntry) Clone() *LogEntry {
	clone := &LogEntry{
		Index: e.Index,
		Term:  e.Term,
		Type:  e.Type,
	}
	if len(e.Data) > 0 {
		clone.Data = make([]byte, len(e.Data))
		copy(clone.Data, e.Data)
	}
	return clone
}

// AppendEntriesRequest is the Raft log replication RPC.
type AppendEntriesRequest struct {
	Term         uint64
	LeaderID     string
	PrevLogIndex uint64
	PrevLogTerm  uint64
	Entries      []*LogEntry
	LeaderCommit uint64
}

// AppendEntriesResponse is the response to AppendEntries.
type AppendEntriesResponse struct {
	Term         uint64
	Success      bool
	LastLogIndex uint64
}

// RequestVoteRequest is the Raft leader election RPC.
type RequestVoteRequest struct {
	Term         uint64
	CandidateID  string
	LastLogIndex uint64
	LastLogTerm  uint64
	IsPreVote    bool // §9.6 Pre-Vote extension.
}

// RequestVoteResponse is the response to RequestVote.
type RequestVoteResponse struct {
	Term        uint64
	VoteGranted bool
	IsPreVote   bool // §9.6 Pre-Vote extension.
}
