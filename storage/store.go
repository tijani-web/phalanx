// Package storage implements the Raft persistence layer backed by BadgerDB.
//
// Design: The LSM-tree stores only small keys and metadata.
// A low ValueThreshold pushes LogEntry data to the value log,
// keeping compaction fast and memory footprint minimal ("Machine Vibe").
//
// Key layout:
//   - "m:term"  → 8-byte big-endian uint64 (currentTerm)
//   - "m:vote"  → string bytes (votedFor)
//   - "l:<8B>"  → JSON-encoded LogEntry (8-byte big-endian index)
package storage

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/dgraph-io/badger/v4"

	"github.com/tijani-web/phalanx/pb"
)

// Fixed key prefixes — single characters minimize LSM key size.
var (
	keyTerm     = []byte("m:term")
	keyVotedFor = []byte("m:vote")
	prefixLog   = []byte("l:")
)

// Store persists Raft state to disk using BadgerDB.
// NOT safe for concurrent use — serialized by the Node event loop.
type Store struct {
	db *badger.DB
}

// New opens a BadgerDB store at the given path.
// Creates the directory if it does not exist.
func New(path string) (*Store, error) {
	opts := badger.DefaultOptions(path)

	// Machine Vibe: values > 64 bytes go to the value log.
	// Keeps LSM-tree compact with only keys + small metadata.
	opts.ValueThreshold = 64

	// Suppress Badger's internal logging — we use slog exclusively.
	opts.Logger = nil

	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("storage: open badger: %w", err)
	}
	return &Store{db: db}, nil
}

// SaveState persists the Raft hard state: currentTerm and votedFor.
// Called after every state transition that changes either value.
func (s *Store) SaveState(term uint64, votedFor string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		// Term: 8-byte big-endian for fixed-width storage.
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, term)
		if err := txn.Set(keyTerm, buf); err != nil {
			return err
		}
		// VotedFor: raw string bytes. Empty string = no vote.
		return txn.Set(keyVotedFor, []byte(votedFor))
	})
}

// LoadState reads the persisted Raft hard state.
// Returns (0, "", nil) if no state has been saved yet (fresh node).
func (s *Store) LoadState() (term uint64, votedFor string, err error) {
	err = s.db.View(func(txn *badger.Txn) error {
		// --- Term ---
		item, e := txn.Get(keyTerm)
		if e == badger.ErrKeyNotFound {
			return nil // default: term = 0
		}
		if e != nil {
			return e
		}
		return item.Value(func(val []byte) error {
			if len(val) == 8 {
				term = binary.BigEndian.Uint64(val)
			}
			return nil
		})
	})
	if err != nil {
		return
	}

	err = s.db.View(func(txn *badger.Txn) error {
		item, e := txn.Get(keyVotedFor)
		if e == badger.ErrKeyNotFound {
			return nil // default: votedFor = ""
		}
		if e != nil {
			return e
		}
		return item.Value(func(val []byte) error {
			votedFor = string(val)
			return nil
		})
	})
	return
}

// logKey returns the BadgerDB key for a log entry at the given index.
// Format: "l:" + 8-byte big-endian index.
// Big-endian ensures lexicographic ordering matches numeric ordering,
// so BadgerDB iteration returns entries in index order.
func logKey(index uint64) []byte {
	key := make([]byte, 2+8) // prefix + 8-byte index
	copy(key, prefixLog)
	binary.BigEndian.PutUint64(key[2:], index)
	return key
}

// AppendEntries persists log entries to disk.
// Each entry is JSON-encoded and keyed by its index.
func (s *Store) AppendEntries(entries []*pb.LogEntry) error {
	return s.db.Update(func(txn *badger.Txn) error {
		for _, entry := range entries {
			data, err := json.Marshal(entry)
			if err != nil {
				return fmt.Errorf("marshal entry %d: %w", entry.Index, err)
			}
			if err := txn.Set(logKey(entry.Index), data); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadLog reads all persisted log entries in index order.
// Returns an empty slice for a fresh node.
func (s *Store) LoadLog() ([]*pb.LogEntry, error) {
	var entries []*pb.LogEntry
	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefixLog
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefixLog); it.ValidForPrefix(prefixLog); it.Next() {
			err := it.Item().Value(func(val []byte) error {
				entry := new(pb.LogEntry)
				if err := json.Unmarshal(val, entry); err != nil {
					return err
				}
				entries = append(entries, entry)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	return entries, err
}

// TruncateFrom removes all log entries with index >= startIndex.
// Used for log conflict resolution on followers.
func (s *Store) TruncateFrom(startIndex uint64) error {
	return s.db.Update(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefixLog
		opts.PrefetchValues = false // keys only
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(logKey(startIndex)); it.ValidForPrefix(prefixLog); it.Next() {
			if err := txn.Delete(it.Item().KeyCopy(nil)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close closes the BadgerDB database.
func (s *Store) Close() error {
	return s.db.Close()
}
