package raft

import (
	"sync/atomic"
	"testing"

	"github.com/basit-tijani/phalanx/logger"
	"github.com/basit-tijani/phalanx/pb"
)

// newTestRaft creates a Raft node with deterministic settings for testing.
func newTestRaft(id string, peers []string) *Raft {
	term := &atomic.Uint64{}
	log := logger.New(id, term)
	return NewRaft(Config{
		ID:               id,
		Peers:            peers,
		ElectionTimeout:  10,
		HeartbeatTimeout: 3,
		Logger:           log,
		Term:             term,
		RandSeed:         42, // deterministic
	})
}

// ---------------------------------------------------------------------------
// Core Tests (Required)
// ---------------------------------------------------------------------------

// TestElectionTimeout verifies that a Follower transitions to Candidate
// after exactly electionTimeout ticks — not one tick earlier, not one later.
func TestElectionTimeout(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})
	timeout := r.ElectionTimeout()

	t.Logf("randomized election timeout: %d ticks", timeout)

	// Tick timeout-1 times: MUST remain Follower.
	for i := 0; i < timeout-1; i++ {
		r.Tick()
		if r.State() != Follower {
			t.Fatalf("tick %d: expected Follower, got %s", i+1, r.State())
		}
	}

	// One more tick: MUST transition to Candidate.
	r.Tick()
	if r.State() != Candidate {
		t.Fatalf("tick %d: expected Candidate, got %s", timeout, r.State())
	}

	// Term should have incremented from 0 → 1.
	if r.CurrentTerm() != 1 {
		t.Errorf("term: want 1, got %d", r.CurrentTerm())
	}

	// Should have broadcast RequestVote to both peers.
	msgs := r.Messages()
	votes := 0
	for _, msg := range msgs {
		if msg.Type == MsgRequestVote {
			votes++
			if msg.From != "node-1" {
				t.Errorf("vote From: want node-1, got %s", msg.From)
			}
			if msg.Term != 1 {
				t.Errorf("vote Term: want 1, got %d", msg.Term)
			}
		}
	}
	if votes != 2 {
		t.Errorf("RequestVote count: want 2, got %d", votes)
	}
}

// TestLeaderHeartbeat verifies that a Leader sends AppendEntries to all
// peers after exactly heartbeatTimeout ticks.
func TestLeaderHeartbeat(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Drive to Candidate state.
	for r.State() != Candidate {
		r.Tick()
	}
	r.Messages() // drain RequestVote messages

	// Win election: one peer vote + self = 2/3 quorum.
	r.Step(Message{
		Type:   MsgRequestVoteResp,
		From:   "node-2",
		To:     "node-1",
		Term:   r.CurrentTerm(),
		Reject: false,
	})

	if r.State() != Leader {
		t.Fatalf("expected Leader after quorum, got %s", r.State())
	}

	// Drain initial heartbeats sent by becomeLeader().
	// becomeLeader now appends a no-op entry (§8), so the initial
	// heartbeats carry that entry.
	initial := r.Messages()
	initialHB := 0
	for _, msg := range initial {
		if msg.Type == MsgAppendEntries {
			initialHB++
			// Verify the no-op entry is included.
			if len(msg.Entries) != 1 {
				t.Errorf("initial heartbeat entries: want 1 (no-op), got %d", len(msg.Entries))
			}
		}
	}
	if initialHB != 2 {
		t.Fatalf("initial heartbeats: want 2, got %d", initialHB)
	}

	// Tick heartbeatTimeout-1 times: NO messages should be generated.
	for i := 0; i < r.heartbeatTimeout-1; i++ {
		r.Tick()
	}
	if msgs := r.Messages(); len(msgs) != 0 {
		t.Fatalf("expected 0 messages before heartbeat timeout, got %d", len(msgs))
	}

	// One more tick: MUST trigger heartbeats to both peers.
	r.Tick()
	msgs := r.Messages()
	heartbeats := 0
	targets := make(map[string]bool)
	for _, msg := range msgs {
		if msg.Type == MsgAppendEntries {
			heartbeats++
			targets[msg.To] = true
			if msg.From != "node-1" {
				t.Errorf("heartbeat From: want node-1, got %s", msg.From)
			}
			if msg.Term != r.CurrentTerm() {
				t.Errorf("heartbeat Term: want %d, got %d", r.CurrentTerm(), msg.Term)
			}
		}
	}
	if heartbeats != 2 {
		t.Errorf("heartbeat count: want 2, got %d", heartbeats)
	}
	if !targets["node-2"] || !targets["node-3"] {
		t.Errorf("missing heartbeat targets: got %v", targets)
	}
}

// ---------------------------------------------------------------------------
// Vote Logic Tests
// ---------------------------------------------------------------------------

// TestPreVote_NoTermChange verifies that Pre-Vote (§9.6) does NOT
// change the receiver's term, votedFor, or state.
func TestPreVote_NoTermChange(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})
	originalTerm := r.CurrentTerm()

	// Pre-vote request from a node with a much higher term.
	r.Step(Message{
		Type:         MsgRequestVote,
		From:         "node-2",
		To:           "node-1",
		Term:         originalTerm + 5,
		LastLogIndex: 0,
		LastLogTerm:  0,
		IsPreVote:    true,
	})

	// Term MUST NOT change.
	if r.CurrentTerm() != originalTerm {
		t.Errorf("term changed during pre-vote: want %d, got %d",
			originalTerm, r.CurrentTerm())
	}

	// votedFor MUST NOT change.
	if r.votedFor != None {
		t.Errorf("votedFor changed during pre-vote: want '', got '%s'", r.votedFor)
	}

	// State MUST NOT change.
	if r.State() != Follower {
		t.Errorf("state changed during pre-vote: want Follower, got %s", r.State())
	}

	// Should have responded with pre-vote GRANTED (our log is empty,
	// candidate's log is equally empty → up-to-date).
	msgs := r.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	resp := msgs[0]
	if resp.Type != MsgRequestVoteResp {
		t.Fatalf("expected RequestVoteResp, got %s", resp.Type)
	}
	if resp.Reject {
		t.Error("pre-vote should have been granted")
	}
	if !resp.IsPreVote {
		t.Error("response should have IsPreVote=true")
	}
}

// TestLogCompleteness verifies that votes are rejected when the
// candidate's log is NOT at least as up-to-date (§5.4.1).
func TestLogCompleteness(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})

	// Give node-1 a log entry at term 5.
	r.log = append(r.log, &pb.LogEntry{
		Index: 1, Term: 5, Type: pb.EntryCommand, Data: []byte("x=1"),
	})

	// Candidate with an OLDER log (term 3, same length) requests vote.
	r.Step(Message{
		Type:         MsgRequestVote,
		From:         "node-2",
		To:           "node-1",
		Term:         6, // Higher term (triggers step-down) but older log.
		LastLogIndex: 1,
		LastLogTerm:  3, // Term 3 < our term 5 at index 1.
	})

	msgs := r.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	if !msgs[0].Reject {
		t.Error("vote should be REJECTED — candidate log (term=3) is less up-to-date than ours (term=5)")
	}
}

// TestVoteGranted verifies that a valid vote request is granted.
func TestVoteGranted(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})

	r.Step(Message{
		Type:         MsgRequestVote,
		From:         "node-2",
		To:           "node-1",
		Term:         1,
		LastLogIndex: 0,
		LastLogTerm:  0,
	})

	msgs := r.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	if msgs[0].Reject {
		t.Error("vote should have been granted — empty logs, valid term")
	}
	if r.votedFor != "node-2" {
		t.Errorf("votedFor: want node-2, got %s", r.votedFor)
	}
}

// TestDoubleVote verifies that a node does not vote for two different
// candidates in the same term.
func TestDoubleVote(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Vote for node-2 in term 1.
	r.Step(Message{
		Type: MsgRequestVote, From: "node-2", To: "node-1",
		Term: 1, LastLogIndex: 0, LastLogTerm: 0,
	})
	msgs := r.Messages()
	if msgs[0].Reject {
		t.Fatal("first vote should be granted")
	}

	// node-3 also requests vote in term 1.
	r.Step(Message{
		Type: MsgRequestVote, From: "node-3", To: "node-1",
		Term: 1, LastLogIndex: 0, LastLogTerm: 0,
	})
	msgs = r.Messages()
	if !msgs[0].Reject {
		t.Error("second vote should be REJECTED — already voted for node-2 in term 1")
	}
}

// ---------------------------------------------------------------------------
// AppendEntries Tests
// ---------------------------------------------------------------------------

// TestAppendEntries_ResetElection verifies that receiving a valid
// AppendEntries resets the election timer, preventing premature elections.
func TestAppendEntries_ResetElection(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})
	timeout := r.ElectionTimeout()

	// Tick to within 2 ticks of the timeout.
	for i := 0; i < timeout-2; i++ {
		r.Tick()
	}

	// Heartbeat from leader — should reset election timer.
	r.Step(Message{
		Type: MsgAppendEntries,
		From: "node-2",
		To:   "node-1",
		Term: r.CurrentTerm(),
	})
	r.Messages() // drain

	// Now tick timeout-2 more times. Without the reset, we'd have
	// exceeded the original timeout. With the reset, we're safe.
	for i := 0; i < timeout-2; i++ {
		r.Tick()
	}

	if r.State() != Follower {
		t.Fatalf("expected Follower after timer reset, got %s", r.State())
	}
}

// TestAppendEntries_CommitIndex verifies that the follower updates
// commitIndex based on the leader's leaderCommit.
func TestAppendEntries_CommitIndex(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})

	// Leader sends 3 entries and commitIndex = 2.
	r.Step(Message{
		Type:         MsgAppendEntries,
		From:         "node-2",
		To:           "node-1",
		Term:         1,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []*pb.LogEntry{
			{Index: 1, Term: 1, Type: pb.EntryCommand, Data: []byte("cmd-1")},
			{Index: 2, Term: 1, Type: pb.EntryCommand, Data: []byte("cmd-2")},
			{Index: 3, Term: 1, Type: pb.EntryCommand, Data: []byte("cmd-3")},
		},
		LeaderCommit: 2,
	})

	// CommitIndex should be min(leaderCommit, lastNewIndex) = min(2, 3) = 2.
	if r.CommitIndex() != 2 {
		t.Errorf("commitIndex: want 2, got %d", r.CommitIndex())
	}

	// Log: sentinel + 3 entries = 4.
	if len(r.Log()) != 4 {
		t.Errorf("log length: want 4, got %d", len(r.Log()))
	}

	// Response should be success with lastLogIndex = 3.
	msgs := r.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	if msgs[0].Reject {
		t.Error("AppendEntries should have succeeded")
	}
	if msgs[0].Index != 3 {
		t.Errorf("response Index: want 3, got %d", msgs[0].Index)
	}
}

// TestAppendEntries_LogConflict verifies that conflicting entries are
// truncated and replaced with the leader's entries.
func TestAppendEntries_LogConflict(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})

	// Pre-populate follower log with entries from a deposed leader.
	r.log = append(r.log,
		&pb.LogEntry{Index: 1, Term: 2, Type: pb.EntryCommand, Data: []byte("old-1")},
		&pb.LogEntry{Index: 2, Term: 2, Type: pb.EntryCommand, Data: []byte("old-2")},
	)

	// New leader sends entries starting at index 1 with a different term.
	r.Step(Message{
		Type:         MsgAppendEntries,
		From:         "node-2",
		To:           "node-1",
		Term:         3,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
		Entries: []*pb.LogEntry{
			{Index: 1, Term: 3, Type: pb.EntryCommand, Data: []byte("new-1")},
			{Index: 2, Term: 3, Type: pb.EntryCommand, Data: []byte("new-2")},
			{Index: 3, Term: 3, Type: pb.EntryCommand, Data: []byte("new-3")},
		},
		LeaderCommit: 0,
	})

	msgs := r.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	if msgs[0].Reject {
		t.Error("AppendEntries should succeed after conflict resolution")
	}

	// Log should have sentinel + 3 new entries.
	if len(r.Log()) != 4 {
		t.Fatalf("log length: want 4, got %d", len(r.Log()))
	}
	// Entry at index 1 should be from the new leader (term 3).
	if r.Log()[1].Term != 3 {
		t.Errorf("log[1].Term: want 3, got %d", r.Log()[1].Term)
	}
	if string(r.Log()[1].Data) != "new-1" {
		t.Errorf("log[1].Data: want 'new-1', got '%s'", r.Log()[1].Data)
	}
}

// ---------------------------------------------------------------------------
// State Transition Tests
// ---------------------------------------------------------------------------

// TestSingleNodeCluster verifies immediate leader promotion
// when there are no peers (quorum = 1).
func TestSingleNodeCluster(t *testing.T) {
	r := newTestRaft("node-1", nil)

	// Tick until election fires.
	for r.State() != Leader {
		r.Tick()
	}

	if r.CurrentTerm() != 1 {
		t.Errorf("term: want 1, got %d", r.CurrentTerm())
	}
	if r.Leader() != "node-1" {
		t.Errorf("leader: want node-1, got %s", r.Leader())
	}

	// No-op entry (§8) should be at index 1.
	if r.LastLogIndex() != 1 {
		t.Errorf("lastLogIndex: want 1 (no-op), got %d", r.LastLogIndex())
	}
	if r.Log()[1].Term != 1 {
		t.Errorf("no-op term: want 1, got %d", r.Log()[1].Term)
	}
	if r.Log()[1].Data != nil {
		t.Errorf("no-op data: want nil, got %v", r.Log()[1].Data)
	}
}

// TestCandidateStepsDown verifies that a Candidate receiving
// AppendEntries from the current term's leader steps down.
func TestCandidateStepsDown(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Drive to Candidate.
	for r.State() != Candidate {
		r.Tick()
	}
	candidateTerm := r.CurrentTerm()
	r.Messages() // drain

	// Another node won the election — sends AE with same term.
	r.Step(Message{
		Type: MsgAppendEntries,
		From: "node-2",
		To:   "node-1",
		Term: candidateTerm,
	})

	if r.State() != Follower {
		t.Fatalf("expected Follower, got %s", r.State())
	}
	if r.Leader() != "node-2" {
		t.Errorf("leader: want node-2, got %s", r.Leader())
	}
}

// TestTermSync verifies the atomic term shared with the logger stays
// in lockstep with the state machine's internal term.
func TestTermSync(t *testing.T) {
	term := &atomic.Uint64{}
	log := logger.New("node-1", term)

	r := NewRaft(Config{
		ID:               "node-1",
		Peers:            []string{"node-2"},
		ElectionTimeout:  5,
		HeartbeatTimeout: 2,
		Logger:           log,
		Term:             term,
	})

	// Initial: term = 0.
	if term.Load() != 0 {
		t.Errorf("initial: want term=0, got %d", term.Load())
	}

	// Trigger election → term = 1.
	for r.State() != Candidate {
		r.Tick()
	}
	if term.Load() != 1 {
		t.Errorf("after election: want term=1, got %d", term.Load())
	}

	// Step down to term 10.
	r.Step(Message{
		Type: MsgAppendEntries,
		From: "node-2",
		To:   "node-1",
		Term: 10,
	})
	r.Messages() // drain

	if term.Load() != 10 {
		t.Errorf("after step-down: want term=10, got %d", term.Load())
	}

	// Raft and atomic MUST be in sync.
	if r.CurrentTerm() != term.Load() {
		t.Errorf("desync: raft.term=%d, atomic.term=%d",
			r.CurrentTerm(), term.Load())
	}
}

// TestLeaderCommitAdvance verifies the leader advances commitIndex
// when a majority of match indices catch up (§5.3, §5.4.2).
func TestLeaderCommitAdvance(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Fast-forward to Leader.
	for r.State() != Candidate {
		r.Tick()
	}
	r.Messages()
	r.Step(Message{
		Type: MsgRequestVoteResp, From: "node-2",
		To: "node-1", Term: r.CurrentTerm(), Reject: false,
	})
	if r.State() != Leader {
		t.Fatalf("expected Leader, got %s", r.State())
	}
	r.Messages()

	// Simulate: leader has entries at index 1 (no-op) + 2, 3, 4.
	// Note: becomeLeader already appended no-op at index 1.
	r.log = append(r.log,
		&pb.LogEntry{Index: 2, Term: r.CurrentTerm(), Type: pb.EntryCommand},
		&pb.LogEntry{Index: 3, Term: r.CurrentTerm(), Type: pb.EntryCommand},
		&pb.LogEntry{Index: 4, Term: r.CurrentTerm(), Type: pb.EntryCommand},
	)
	// Reset nextIndex for followers.
	for _, p := range r.peers {
		r.nextIndex[p] = 5
	}

	// node-2 confirms replication up to index 3.
	r.Step(Message{
		Type:   MsgAppendEntriesResp,
		From:   "node-2",
		To:     "node-1",
		Term:   r.CurrentTerm(),
		Reject: false,
		Index:  3,
	})

	// Quorum: self (4) + node-2 (3) → majority at index 3.
	if r.CommitIndex() != 3 {
		t.Errorf("commitIndex: want 3, got %d", r.CommitIndex())
	}

	// node-3 confirms replication up to index 4.
	r.Step(Message{
		Type:   MsgAppendEntriesResp,
		From:   "node-3",
		To:     "node-1",
		Term:   r.CurrentTerm(),
		Reject: false,
		Index:  4,
	})

	// All nodes at index 4 → commit advances to 4.
	if r.CommitIndex() != 4 {
		t.Errorf("commitIndex: want 4, got %d", r.CommitIndex())
	}
}

// ---------------------------------------------------------------------------
// Hardening Tests (Pre-Phase 4)
// ---------------------------------------------------------------------------

// TestNoOpEntry verifies that becomeLeader appends a no-op entry (§8)
// and broadcasts it in the initial heartbeats.
func TestNoOpEntry(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Drive to Leader.
	for r.State() != Candidate {
		r.Tick()
	}
	r.Messages()
	r.Step(Message{
		Type: MsgRequestVoteResp, From: "node-2",
		To: "node-1", Term: r.CurrentTerm(), Reject: false,
	})
	if r.State() != Leader {
		t.Fatalf("expected Leader, got %s", r.State())
	}

	// No-op should be the last entry in the log.
	noop := r.Log()[r.LastLogIndex()]
	if noop.Term != r.CurrentTerm() {
		t.Errorf("no-op term: want %d, got %d", r.CurrentTerm(), noop.Term)
	}
	if noop.Data != nil {
		t.Errorf("no-op data: want nil, got %v", noop.Data)
	}
	if noop.Type != pb.EntryCommand {
		t.Errorf("no-op type: want COMMAND, got %d", noop.Type)
	}

	// Initial heartbeats should carry the no-op entry.
	msgs := r.Messages()
	for _, msg := range msgs {
		if msg.Type == MsgAppendEntries {
			if len(msg.Entries) == 0 {
				t.Errorf("heartbeat to %s has no entries (should carry no-op)", msg.To)
			} else if msg.Entries[0].Term != r.CurrentTerm() {
				t.Errorf("no-op entry term: want %d, got %d", r.CurrentTerm(), msg.Entries[0].Term)
			}
		}
	}
}

// TestPreVoteLiveness verifies that a quorum of pre-vote grants
// triggers a real election (becomeCandidate with term increment).
func TestPreVoteLiveness(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Manually set state to Candidate (simulates a pre-vote phase).
	r.state = Candidate
	r.currentTerm = 1
	r.syncTerm()

	termBefore := r.CurrentTerm()

	// Feed pre-vote grant from node-2 (self + node-2 = quorum of 2/3).
	r.Step(Message{
		Type:      MsgRequestVoteResp,
		From:      "node-2",
		To:        "node-1",
		Term:      termBefore,
		Reject:    false,
		IsPreVote: true,
	})

	// Pre-vote quorum should trigger becomeCandidate() → term increments.
	if r.CurrentTerm() != termBefore+1 {
		t.Errorf("term after pre-vote quorum: want %d, got %d",
			termBefore+1, r.CurrentTerm())
	}

	// Should now be a real Candidate with RequestVote messages (not pre-vote).
	msgs := r.Messages()
	votes := 0
	for _, msg := range msgs {
		if msg.Type == MsgRequestVote && !msg.IsPreVote {
			votes++
		}
	}
	if votes != 2 {
		t.Errorf("real RequestVote count: want 2, got %d", votes)
	}
}

// TestLeaderStickiness verifies that a node with an active leader
// rejects vote requests to prevent disruptive elections.
func TestLeaderStickiness(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2", "node-3"})

	// Receive a heartbeat from the leader — marks leader as active.
	r.Step(Message{
		Type: MsgAppendEntries,
		From: "node-2",
		To:   "node-1",
		Term: 1,
	})
	r.Messages() // drain

	if !r.leaderActive {
		t.Fatal("leaderActive should be true after receiving AppendEntries")
	}

	// node-3 requests a real vote at the same term. Should be REJECTED
	// because the leader is still active.
	r.Step(Message{
		Type:         MsgRequestVote,
		From:         "node-3",
		To:           "node-1",
		Term:         r.CurrentTerm(),
		LastLogIndex: 0,
		LastLogTerm:  0,
	})
	msgs := r.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 response, got %d", len(msgs))
	}
	if !msgs[0].Reject {
		t.Error("vote should be REJECTED — leader is active")
	}

	// Pre-vote should also be rejected.
	r.Step(Message{
		Type:         MsgRequestVote,
		From:         "node-3",
		To:           "node-1",
		Term:         r.CurrentTerm(),
		LastLogIndex: 0,
		LastLogTerm:  0,
		IsPreVote:    true,
	})
	msgs = r.Messages()
	if !msgs[0].Reject {
		t.Error("pre-vote should also be REJECTED — leader is active")
	}
	if !msgs[0].IsPreVote {
		t.Error("response should have IsPreVote=true")
	}
}

// TestHeartbeatMemorySafety verifies that entries in heartbeat messages
// are deep copies, not references to the internal log.
func TestHeartbeatMemorySafety(t *testing.T) {
	r := newTestRaft("node-1", []string{"node-2"})

	// Drive to Leader.
	for r.State() != Candidate {
		r.Tick()
	}
	r.Messages()
	r.Step(Message{
		Type: MsgRequestVoteResp, From: "node-2",
		To: "node-1", Term: r.CurrentTerm(), Reject: false,
	})
	if r.State() != Leader {
		t.Fatalf("expected Leader, got %s", r.State())
	}

	// Add a data-bearing entry to the log.
	data := []byte("critical-state")
	r.log = append(r.log, &pb.LogEntry{
		Index: r.LastLogIndex() + 1,
		Term:  r.CurrentTerm(),
		Type:  pb.EntryCommand,
		Data:  data,
	})
	// Reset nextIndex so heartbeat includes this entry.
	r.nextIndex["node-2"] = 1

	r.Messages() // drain initial

	// Trigger heartbeat.
	for i := 0; i < r.heartbeatTimeout; i++ {
		r.Tick()
	}
	msgs := r.Messages()

	// Find the AE message to node-2.
	var found bool
	for _, msg := range msgs {
		if msg.Type == MsgAppendEntries && msg.To == "node-2" {
			for _, entry := range msg.Entries {
				if len(entry.Data) > 0 {
					// Mutate the original log data.
					data[0] = 'X'
					// Entry in message must NOT be affected.
					if entry.Data[0] == 'X' {
						t.Fatal("MEMORY SAFETY VIOLATION: heartbeat entry shares backing array with internal log")
					}
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("no data-bearing entry found in heartbeat")
	}
}
