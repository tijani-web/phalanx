// Package raft implements the core Raft consensus state machine for Phalanx.
//
// Design: The state machine is pure — it produces Messages via Step()/Tick()
// but does not send them. Callers read Messages() after each operation and
// dispatch them via the Transport layer. This makes the state machine fully
// deterministic and testable without real networking.
//
// References:
//   - Raft paper §5: Core consensus
//   - Raft paper §6: Cluster membership changes (Phase 4)
//   - Raft paper §9.6: Pre-Vote extension
package raft

import (
	"errors"
	"log/slog"
	"math/rand"
	"sync/atomic"

	"github.com/tijani-web/phalanx/pb"
)

// ---------------------------------------------------------------------------
// State & Message Types
// ---------------------------------------------------------------------------

// NodeState represents the Raft state machine state.
type NodeState uint8

const (
	Follower NodeState = iota
	Candidate
	Leader
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "FOLLOWER"
	case Candidate:
		return "CANDIDATE"
	case Leader:
		return "LEADER"
	default:
		return "UNKNOWN"
	}
}

// MessageType identifies the Raft RPC type.
type MessageType uint8

const (
	MsgRequestVote MessageType = iota + 1
	MsgRequestVoteResp
	MsgAppendEntries
	MsgAppendEntriesResp
)

func (m MessageType) String() string {
	switch m {
	case MsgRequestVote:
		return "RequestVote"
	case MsgRequestVoteResp:
		return "RequestVoteResp"
	case MsgAppendEntries:
		return "AppendEntries"
	case MsgAppendEntriesResp:
		return "AppendEntriesResp"
	default:
		return "Unknown"
	}
}

// None represents an empty node identity (no leader / no vote).
const None = ""

// ---------------------------------------------------------------------------
// Message
// ---------------------------------------------------------------------------

// Message is the unit of communication between Raft peers.
// The state machine produces these; a higher-level component dispatches
// them via the Transport layer (Phase 5 integration).
type Message struct {
	Type MessageType
	From string
	To   string
	Term uint64

	// AppendEntries request fields.
	PrevLogIndex uint64
	PrevLogTerm  uint64
	Entries      []*pb.LogEntry
	LeaderCommit uint64

	// RequestVote request fields.
	LastLogIndex uint64
	LastLogTerm  uint64
	IsPreVote    bool // §9.6: pre-vote doesn't change state.

	// Response fields.
	Reject bool   // true = vote denied / append rejected.
	Index  uint64 // Follower's lastLogIndex (AE resp) or conflict hint.
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config defines the parameters for a Raft node.
type Config struct {
	ID               string         // Unique node identifier — must match logger's node_id.
	Peers            []string       // Other node IDs in the cluster.
	ElectionTimeout  int            // Base election timeout in ticks. Randomized to [ET, 2*ET).
	HeartbeatTimeout int            // Heartbeat interval in ticks. Must be << ElectionTimeout.
	Logger           *slog.Logger   // Structured logger from the logger package.
	Term             *atomic.Uint64 // Shared with logger for live term injection.
	RandSeed         int64          // Seed for deterministic testing. 0 = derive from ID.

	// State restoration from persistent storage (Phase 4).
	// Zero values are safe — they represent a fresh node.
	InitialTerm     uint64         // Persisted currentTerm.
	InitialVotedFor string         // Persisted votedFor.
	InitialLog      []*pb.LogEntry // Persisted log entries (excluding sentinel).
}

// ---------------------------------------------------------------------------
// Raft
// ---------------------------------------------------------------------------

// Raft implements the core Raft consensus state machine.
//
// NOT safe for concurrent use — callers must serialize access.
// This is by design: the event-loop model (Tick → Step → Messages)
// eliminates lock contention on the hot path.
type Raft struct {
	id string

	// Persistent state on all servers (Figure 2).
	currentTerm uint64
	votedFor    string
	log         []*pb.LogEntry // Index 0 is a sentinel entry.

	// Volatile state on all servers.
	commitIndex uint64
	lastApplied uint64

	// Volatile state on leaders (reinitialized after election).
	nextIndex  map[string]uint64
	matchIndex map[string]uint64

	// State machine.
	state    NodeState
	leaderID string

	// Tick-based deterministic timing (no time.Ticker).
	electionElapsed     int
	heartbeatElapsed    int
	electionTimeout     int // randomized per transition: [base, 2*base)
	heartbeatTimeout    int
	baseElectionTimeout int

	// Heartbeat ack tracking for lease-based linearizable reads.
	// Reset each heartbeat round; incremented on AE success.
	heartbeatAcked int

	// Leader stickiness: tracks how recently we heard from a valid leader.
	// If leaderActive is true, we reject vote requests to prevent disruptive
	// elections from partitioned nodes rejoining the cluster.
	leaderActive bool

	// Election tracking.
	votes    map[string]bool
	preVotes map[string]bool // Pre-vote tracking (§9.6).

	// Cluster configuration.
	peers []string

	// Output buffer — outbound messages accumulated since last Messages() call.
	msgs []Message

	// Dependencies.
	logger *slog.Logger
	term   *atomic.Uint64 // shared with logger for automatic term injection
	rand   *rand.Rand
}

// NewRaft creates a new Raft node in the Follower state.
// The election timeout is randomized from the base to prevent election storms.
func NewRaft(cfg Config) *Raft {
	seed := cfg.RandSeed
	if seed == 0 {
		// Derive from ID so different nodes get different timeout distributions.
		// Prevents correlated elections in the cluster.
		seed = int64(djb2(cfg.ID))
	}

	r := &Raft{
		id:                  cfg.ID,
		votedFor:            None,
		leaderID:            None,
		heartbeatTimeout:    cfg.HeartbeatTimeout,
		baseElectionTimeout: cfg.ElectionTimeout,
		peers:               cfg.Peers,
		logger:              cfg.Logger,
		term:                cfg.Term,
		rand:                rand.New(rand.NewSource(seed)),
	}

	// Sentinel entry at index 0 — simplifies all log indexing.
	r.log = []*pb.LogEntry{{Index: 0, Term: 0, Type: pb.EntryCommand}}

	// Restore persisted state from storage (Phase 4).
	if cfg.InitialTerm > 0 {
		r.currentTerm = cfg.InitialTerm
		r.syncTerm()
	}
	if cfg.InitialVotedFor != "" {
		r.votedFor = cfg.InitialVotedFor
	}
	if len(cfg.InitialLog) > 0 {
		r.log = append(r.log, cfg.InitialLog...)
	}

	r.resetElectionTimeout()

	r.logger.Info("raft initialized",
		"peers", cfg.Peers,
		"election_timeout", r.electionTimeout,
		"heartbeat_timeout", r.heartbeatTimeout,
	)

	return r
}

// djb2 hashes a string to a uint64 for deterministic seeding.
func djb2(s string) uint64 {
	var h uint64 = 5381
	for i := 0; i < len(s); i++ {
		h = (h << 5) + h + uint64(s[i])
	}
	return h
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Tick advances the internal logical clock by one tick.
//
//   - Follower/Candidate: increments electionElapsed → triggers election on timeout.
//   - Leader: increments heartbeatElapsed → broadcasts heartbeats on timeout.
func (r *Raft) Tick() {
	switch r.state {
	case Follower, Candidate:
		r.tickElection()
	case Leader:
		r.tickHeartbeat()
	}
}

// Step processes an inbound message from a peer.
// After calling Step, read Messages() to get the outbound responses.
func (r *Raft) Step(msg Message) error {
	// §5.1: If RPC term > currentTerm, step down to follower.
	// Exception: pre-vote messages don't trigger term changes (§9.6).
	if msg.Term > r.currentTerm {
		isPreVote := msg.IsPreVote &&
			(msg.Type == MsgRequestVote || msg.Type == MsgRequestVoteResp)

		if !isPreVote {
			leader := None
			if msg.Type == MsgAppendEntries {
				leader = msg.From
			}
			r.logger.Info("received higher term, stepping down",
				"msg_type", msg.Type.String(),
				"from", msg.From,
				"msg_term", msg.Term,
			)
			r.becomeFollower(msg.Term, leader)
		}
	}

	switch msg.Type {
	case MsgRequestVote:
		r.handleRequestVote(msg)
	case MsgRequestVoteResp:
		r.handleRequestVoteResp(msg)
	case MsgAppendEntries:
		r.handleAppendEntries(msg)
	case MsgAppendEntriesResp:
		r.handleAppendEntriesResp(msg)
	}

	return nil
}

// Messages returns and clears the outbound message buffer.
// The caller dispatches these via the Transport layer.
func (r *Raft) Messages() []Message {
	msgs := r.msgs
	r.msgs = nil
	return msgs
}

// State returns the current Raft node state.
func (r *Raft) State() NodeState { return r.state }

// Leader returns the current leader's node ID (or None).
func (r *Raft) Leader() string { return r.leaderID }

// CurrentTerm returns the current Raft term.
func (r *Raft) CurrentTerm() uint64 { return r.currentTerm }

// CommitIndex returns the index of the highest committed log entry.
func (r *Raft) CommitIndex() uint64 { return r.commitIndex }

// ElectionTimeout returns the current randomized election timeout in ticks.
func (r *Raft) ElectionTimeout() int { return r.electionTimeout }

// Log returns the log entries (including sentinel at index 0).
func (r *Raft) Log() []*pb.LogEntry { return r.log }

// LastLogIndex returns the index of the last log entry.
func (r *Raft) LastLogIndex() uint64 { return r.lastLogIndex() }

// LastLogTerm returns the term of the last log entry.
func (r *Raft) LastLogTerm() uint64 { return r.lastLogTerm() }

// VotedFor returns the node ID this node voted for in the current term.
// Returns None ("") if no vote has been cast.
func (r *Raft) VotedFor() string { return r.votedFor }

// LastApplied returns the index of the last applied log entry.
func (r *Raft) LastApplied() uint64 { return r.lastApplied }

// Peers returns the peer node IDs.
func (r *Raft) Peers() []string { return r.peers }

// ErrNotLeader is returned when a client submits a proposal to a non-leader.
var ErrNotLeader = errors.New("raft: not leader")

// Propose appends a client command to the leader's log and broadcasts
// it to followers. Returns the index of the proposed entry.
// Returns ErrNotLeader if this node is not the leader.
func (r *Raft) Propose(data []byte) (uint64, error) {
	if r.state != Leader {
		return 0, ErrNotLeader
	}
	entry := &pb.LogEntry{
		Index: r.lastLogIndex() + 1,
		Term:  r.currentTerm,
		Type:  pb.EntryCommand,
		Data:  data,
	}
	r.log = append(r.log, entry)

	// Leader's own matchIndex always tracks the end of its log.
	if r.matchIndex != nil {
		r.matchIndex[r.id] = entry.Index
	}

	r.logger.Debug("proposed entry", "index", entry.Index)
	r.broadcastHeartbeat()
	return entry.Index, nil
}

// ApplicableEntries returns committed entries that haven't been applied
// to the state machine yet. Advances lastApplied to commitIndex.
// The caller applies these to the FSM in order.
func (r *Raft) ApplicableEntries() []*pb.LogEntry {
	if r.lastApplied >= r.commitIndex {
		return nil
	}
	// Guard against out-of-bounds if commitIndex is beyond our log
	// (can happen briefly on followers before replication catches up).
	end := r.commitIndex
	if end >= uint64(len(r.log)) {
		end = uint64(len(r.log)) - 1
	}
	if r.lastApplied >= end {
		return nil
	}
	entries := make([]*pb.LogEntry, 0, end-r.lastApplied)
	for i := r.lastApplied + 1; i <= end; i++ {
		entries = append(entries, r.log[i])
	}
	r.lastApplied = end
	return entries
}

// HasLeaderQuorum returns true if this node is the leader AND has
// received heartbeat acks from a majority in the current round.
// Used for lease-based linearizable reads — the leader can serve
// reads only if it can confirm it still holds authority.
func (r *Raft) HasLeaderQuorum() bool {
	return r.state == Leader && r.heartbeatAcked >= r.quorumSize()
}

// ---------------------------------------------------------------------------
// Tick Handlers
// ---------------------------------------------------------------------------

func (r *Raft) tickElection() {
	r.electionElapsed++
	if r.electionElapsed >= r.electionTimeout {
		r.electionElapsed = 0
		// Leader stickiness expires: we haven't heard from the leader
		// for a full election timeout. The leader is presumed dead.
		r.leaderActive = false
		r.becomeCandidate()
	}
}

func (r *Raft) tickHeartbeat() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		// Reset ack counter for this round. Self counts as 1.
		r.heartbeatAcked = 1
		r.broadcastHeartbeat()
	}
}

// ---------------------------------------------------------------------------
// State Transitions
// ---------------------------------------------------------------------------

func (r *Raft) becomeFollower(term uint64, leaderID string) {
	r.state = Follower
	r.currentTerm = term
	r.votedFor = None
	r.leaderID = leaderID
	r.leaderActive = leaderID != None
	r.electionElapsed = 0
	r.resetElectionTimeout()
	r.syncTerm()
	r.logger.Info("became follower", "leader", leaderID)
}

func (r *Raft) becomeCandidate() {
	r.state = Candidate
	r.currentTerm++
	r.votedFor = r.id
	r.leaderID = None
	r.votes = make(map[string]bool)
	r.votes[r.id] = true // Self-vote.
	r.electionElapsed = 0
	r.resetElectionTimeout()
	r.syncTerm()

	r.logger.Info("became candidate", "quorum_needed", r.quorumSize())

	// Single-node cluster: immediate leader promotion.
	if r.quorum() {
		r.becomeLeader()
		return
	}

	r.broadcastRequestVote()
}

func (r *Raft) becomeLeader() {
	r.state = Leader
	r.leaderID = r.id
	r.heartbeatElapsed = 0
	r.heartbeatAcked = 1 // Self.

	// Initialize leader volatile state (§5.3).
	lastIdx := r.lastLogIndex()
	r.nextIndex = make(map[string]uint64)
	r.matchIndex = make(map[string]uint64)
	for _, peer := range r.peers {
		r.nextIndex[peer] = lastIdx + 1
		r.matchIndex[peer] = 0
	}

	// --- No-Op Entry (§8) ---
	// Append an empty COMMAND entry in the leader's current term.
	// This is CRITICAL: without it, the leader cannot determine which
	// entries from prior terms are committed, because §5.4.2 only
	// allows committing entries from the current term. The no-op
	// entry "unlocks" the commit pipeline for all prior-term entries.
	noop := &pb.LogEntry{
		Index: lastIdx + 1,
		Term:  r.currentTerm,
		Type:  pb.EntryCommand,
		Data:  nil, // Empty — this is a marker, not a client command.
	}
	r.log = append(r.log, noop)

	// NOTE: nextIndex was already set to lastIdx + 1 above, which equals
	// noop.Index. This means broadcastHeartbeat will include the no-op
	// in the initial AppendEntries — exactly what we want.

	r.logger.Info("became leader", "noop_index", noop.Index)

	// Immediate heartbeats to establish authority, replicate the no-op,
	// and prevent new elections.
	r.broadcastHeartbeat()
}

// ---------------------------------------------------------------------------
// Message Handlers
// ---------------------------------------------------------------------------

func (r *Raft) handleRequestVote(msg Message) {
	reply := Message{
		Type:   MsgRequestVoteResp,
		From:   r.id,
		To:     msg.From,
		Term:   r.currentTerm,
		Reject: true,
	}

	// --- Leader Stickiness ---
	// If we've recently heard from a valid leader (within the election
	// timeout), reject ALL vote requests — pre-vote or real.
	// This prevents a partitioned node from disrupting a stable cluster
	// by starting elections the moment it reconnects.
	if r.leaderActive && msg.Term <= r.currentTerm {
		r.logger.Info("ignoring vote request: leader is active",
			"from", msg.From, "is_prevote", msg.IsPreVote)
		if msg.IsPreVote {
			reply.IsPreVote = true
		}
		r.send(reply)
		return
	}

	// --- Pre-Vote (§9.6) ---
	// Don't change state. Act as a read-only query:
	// "Would you vote for me if I started an election at this term?"
	if msg.IsPreVote {
		canGrant := msg.Term >= r.currentTerm &&
			r.isLogUpToDate(msg.LastLogIndex, msg.LastLogTerm)
		reply.Reject = !canGrant
		reply.IsPreVote = true
		r.send(reply)
		return
	}

	// Reject stale terms outright.
	if msg.Term < r.currentTerm {
		r.send(reply)
		return
	}

	// --- Real Vote (§5.2, §5.4.1) ---
	// Grant if: (a) we haven't voted (or voted for this candidate),
	// AND (b) candidate's log is at least as up-to-date.
	canVote := r.votedFor == None || r.votedFor == msg.From
	logOK := r.isLogUpToDate(msg.LastLogIndex, msg.LastLogTerm)

	if canVote && logOK {
		r.votedFor = msg.From
		r.electionElapsed = 0 // Reset timer on vote grant (§5.2).
		reply.Reject = false
		r.logger.Info("granted vote", "to", msg.From)
	} else {
		r.logger.Debug("rejected vote", "to", msg.From,
			"can_vote", canVote, "log_ok", logOK)
	}

	r.send(reply)
}

func (r *Raft) handleRequestVoteResp(msg Message) {
	if r.state != Candidate {
		return // Stale — we're no longer a candidate.
	}

	// --- Pre-Vote Liveness (§9.6) ---
	// Count pre-vote responses separately. If a quorum of pre-votes
	// is reached, the node transitions to a real Candidate election.
	// This ensures liveness: a node with stale data won't spin forever
	// in pre-vote limbo if the cluster is actually available.
	if msg.IsPreVote {
		if r.preVotes == nil {
			r.preVotes = make(map[string]bool)
			r.preVotes[r.id] = true // Self pre-vote.
		}
		r.preVotes[msg.From] = !msg.Reject

		if !msg.Reject {
			r.logger.Debug("pre-vote received", "from", msg.From)
		}

		// Check if pre-vote quorum reached.
		preGranted := 0
		for _, v := range r.preVotes {
			if v {
				preGranted++
			}
		}
		if preGranted >= r.quorumSize() {
			r.logger.Info("pre-vote quorum reached, starting real election")
			// Transition to real Candidate: increment term, broadcast real votes.
			r.becomeCandidate()
		}
		return
	}

	if msg.Term != r.currentTerm {
		return // Stale response from a different term.
	}

	r.votes[msg.From] = !msg.Reject

	if !msg.Reject {
		r.logger.Debug("vote received", "from", msg.From)
	}

	if r.quorum() {
		r.becomeLeader()
	}
}

func (r *Raft) handleAppendEntries(msg Message) {
	reply := Message{
		Type:   MsgAppendEntriesResp,
		From:   r.id,
		To:     msg.From,
		Term:   r.currentTerm,
		Reject: true,
	}

	// Reject stale leader.
	if msg.Term < r.currentTerm {
		r.send(reply)
		return
	}

	// Valid AppendEntries — reset election timer and mark leader as active.
	r.electionElapsed = 0
	r.leaderID = msg.From
	r.leaderActive = true

	// Candidate or Leader receiving AE: step down.
	// (Two leaders in the same term is impossible in correct Raft,
	// but we handle it defensively for split-brain safety.)
	if r.state != Follower {
		r.becomeFollower(msg.Term, msg.From)
		reply.Term = r.currentTerm
	}

	// --- Log Consistency Check (§5.3) ---
	if msg.PrevLogIndex > 0 {
		if msg.PrevLogIndex > r.lastLogIndex() {
			// Gap: we're missing entries. Hint our last index for fast catchup.
			reply.Index = r.lastLogIndex()
			r.send(reply)
			return
		}
		if r.logTerm(msg.PrevLogIndex) != msg.PrevLogTerm {
			// Conflict: terms don't match. Hint start of conflicting term
			// for accelerated backtracking (optimization over decrement-by-one).
			conflictTerm := r.logTerm(msg.PrevLogIndex)
			hint := msg.PrevLogIndex
			for hint > 1 && r.logTerm(hint-1) == conflictTerm {
				hint--
			}
			reply.Index = hint - 1
			r.send(reply)
			return
		}
	}

	// --- Append Entries (§5.3) ---
	for i, entry := range msg.Entries {
		idx := msg.PrevLogIndex + uint64(i) + 1
		if idx <= r.lastLogIndex() {
			if r.logTerm(idx) != entry.Term {
				// Conflict: truncate from here and append remaining.
				r.log = r.log[:idx]
				r.log = append(r.log, msg.Entries[i:]...)
				break
			}
			// Matching entry — skip.
		} else {
			// Past the end — append all remaining.
			r.log = append(r.log, msg.Entries[i:]...)
			break
		}
	}

	// --- Update CommitIndex (§5.3) ---
	if msg.LeaderCommit > r.commitIndex {
		lastNewIdx := msg.PrevLogIndex + uint64(len(msg.Entries))
		if msg.LeaderCommit < lastNewIdx {
			r.commitIndex = msg.LeaderCommit
		} else {
			r.commitIndex = lastNewIdx
		}
	}

	reply.Reject = false
	reply.Index = r.lastLogIndex()
	r.send(reply)
}

func (r *Raft) handleAppendEntriesResp(msg Message) {
	if r.state != Leader {
		return
	}
	if msg.Term != r.currentTerm {
		return // Stale.
	}

	if msg.Reject {
		// Back off nextIndex using the conflict hint for fast catchup.
		if msg.Index > 0 {
			r.nextIndex[msg.From] = msg.Index + 1
		} else if r.nextIndex[msg.From] > 1 {
			r.nextIndex[msg.From]--
		}
		r.logger.Debug("append rejected, backing off",
			"from", msg.From,
			"next_index", r.nextIndex[msg.From],
		)
		return
	}

	// Success — advance nextIndex and matchIndex.
	if msg.Index > 0 {
		r.matchIndex[msg.From] = msg.Index
		r.nextIndex[msg.From] = msg.Index + 1
	}

	// Count successful ack for lease-based reads.
	r.heartbeatAcked++

	r.maybeAdvanceCommit()
}

// ---------------------------------------------------------------------------
// Broadcast
// ---------------------------------------------------------------------------

func (r *Raft) broadcastRequestVote() {
	lastIdx := r.lastLogIndex()
	lastTerm := r.lastLogTerm()

	for _, peer := range r.peers {
		r.send(Message{
			Type:         MsgRequestVote,
			From:         r.id,
			To:           peer,
			Term:         r.currentTerm,
			LastLogIndex: lastIdx,
			LastLogTerm:  lastTerm,
		})
	}
}

func (r *Raft) broadcastHeartbeat() {
	for _, peer := range r.peers {
		prevIdx := r.nextIndex[peer] - 1
		prevTerm := r.logTerm(prevIdx)

		// --- Memory Safety ---
		// Deep-copy entries via the pool instead of slicing into the
		// internal log. Sharing slice headers with the log means any
		// subsequent append/truncate on r.log could silently corrupt
		// in-flight messages that haven't been dispatched yet.
		var entries []*pb.LogEntry
		if r.nextIndex[peer] <= r.lastLogIndex() {
			src := r.log[r.nextIndex[peer]:]
			entries = make([]*pb.LogEntry, len(src))
			for i, e := range src {
				entries[i] = e.Clone() // Deep copy via pb.LogEntry.Clone().
			}
		}

		r.send(Message{
			Type:         MsgAppendEntries,
			From:         r.id,
			To:           peer,
			Term:         r.currentTerm,
			PrevLogIndex: prevIdx,
			PrevLogTerm:  prevTerm,
			Entries:      entries,
			LeaderCommit: r.commitIndex,
		})
	}
}

// ---------------------------------------------------------------------------
// Log Helpers
// ---------------------------------------------------------------------------

func (r *Raft) lastLogIndex() uint64 {
	return r.log[len(r.log)-1].Index
}

func (r *Raft) lastLogTerm() uint64 {
	return r.log[len(r.log)-1].Term
}

// logTerm returns the term of the entry at the given index.
// Returns 0 for out-of-bounds indices and the sentinel.
func (r *Raft) logTerm(index uint64) uint64 {
	if index >= uint64(len(r.log)) {
		return 0
	}
	return r.log[index].Term
}

// isLogUpToDate returns true if (lastIndex, lastTerm) is at least
// as up-to-date as this node's log (§5.4.1).
//
// The log with the later term wins. If terms are equal, the longer log wins.
func (r *Raft) isLogUpToDate(lastIndex, lastTerm uint64) bool {
	ourTerm := r.lastLogTerm()
	ourIndex := r.lastLogIndex()
	if lastTerm != ourTerm {
		return lastTerm > ourTerm
	}
	return lastIndex >= ourIndex
}

// ---------------------------------------------------------------------------
// Quorum
// ---------------------------------------------------------------------------

func (r *Raft) clusterSize() int {
	return len(r.peers) + 1 // peers + self
}

func (r *Raft) quorumSize() int {
	return r.clusterSize()/2 + 1
}

// quorum returns true if we have enough granted votes to win the election.
//
// The self-vote is included because becomeCandidate() sets votes[r.id] = true
// before calling this function. This means the self-vote is always counted
// in the granted tally, and we only need (quorumSize - 1) additional peer
// votes to reach majority. For a 3-node cluster: quorumSize=2, self-vote=1,
// so 1 peer grant is sufficient.
func (r *Raft) quorum() bool {
	granted := 0
	for _, v := range r.votes {
		if v {
			granted++
		}
	}
	return granted >= r.quorumSize()
}

// maybeAdvanceCommit advances commitIndex if a majority have replicated
// up to a certain index AND the entry is from the current term (§5.4.2).
func (r *Raft) maybeAdvanceCommit() {
	for n := r.commitIndex + 1; n <= r.lastLogIndex(); n++ {
		if r.logTerm(n) != r.currentTerm {
			continue // Only commit entries from current term (§5.4.2).
		}
		matches := 1 // Self.
		for _, peer := range r.peers {
			if r.matchIndex[peer] >= n {
				matches++
			}
		}
		if matches >= r.quorumSize() {
			r.commitIndex = n
		}
	}
}

// ---------------------------------------------------------------------------
// Internal Helpers
// ---------------------------------------------------------------------------

func (r *Raft) send(msg Message) {
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) resetElectionTimeout() {
	// Randomize to [base, 2*base) to prevent correlated elections.
	r.electionTimeout = r.baseElectionTimeout + r.rand.Intn(r.baseElectionTimeout)
}

// syncTerm updates the atomic term shared with the logger.
// After this call, all log lines from this node reflect the new term.
func (r *Raft) syncTerm() {
	r.term.Store(r.currentTerm)
}

// ProposeConfigChange appends a CONFIG_CHANGE log entry to propose
// adding a new node to the cluster. Only the Leader can propose.
// The entry is replicated via the next heartbeat broadcast.
//
// This is a simplified single-change approach.
// Full joint-consensus (§6) is deferred to Phase 5.
func (r *Raft) ProposeConfigChange(addr string) {
	if r.state != Leader {
		r.logger.Warn("config change rejected: not leader", "addr", addr)
		return
	}

	entry := &pb.LogEntry{
		Index: r.lastLogIndex() + 1,
		Term:  r.currentTerm,
		Type:  pb.EntryConfigChange,
		Data:  []byte(addr),
	}
	r.log = append(r.log, entry)

	r.logger.Info("proposed config change",
		"addr", addr,
		"index", entry.Index,
	)

	// Replicate to followers immediately.
	r.broadcastHeartbeat()
}
