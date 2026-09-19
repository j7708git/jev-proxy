// Package store appends scoring events to a monthly JSONL file and serves
// lookups by response_id from an in-memory index (rebuilt lazily on start).
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one line of scores-YYYY-MM.jsonl. Fields follow the plan doc;
// response_id is the proxy-generated key returned as X-Jev-Request-Id.
type Event struct {
	TS            time.Time   `json:"ts"`
	ResponseID    string      `json:"response_id"`
	UpstreamID    string      `json:"upstream_id,omitempty"`
	UpstreamModel string      `json:"upstream_model,omitempty"`
	RubricVersion string      `json:"rubric_version"`
	JevModel      string      `json:"jev_model,omitempty"`
	Weighted      *float64    `json:"weighted"`
	APIScore      *float64    `json:"api_score,omitempty"`
	LevelProbs    *[4]float64 `json:"level_probs,omitempty"`
	Confidence    *float64    `json:"confidence,omitempty"`
	LowConfidence bool        `json:"low_confidence"`
	SafetyProb    *float64    `json:"safety_prob,omitempty"`
	SafetyFlag    bool        `json:"safety_flag"`
	FlagReview    bool        `json:"flag_review"`
	SuspectL0     bool        `json:"suspect_l0"`
	Status        string      `json:"status"` // ok | skipped | error
	Reason        string      `json:"reason,omitempty"`
	LatencyMS     int64       `json:"latency_ms"`
	ReplySHA256   string      `json:"reply_sha256,omitempty"`
	Reply         string      `json:"reply,omitempty"`
	Error         string      `json:"error,omitempty"`
}

// Store is safe for concurrent use.
type Store struct {
	mu    sync.Mutex
	dir   string
	base  string // file name prefix, e.g. "scores"
	month string // "2006-01" of the currently open file
	f     *os.File
	w     *bufio.Writer
	byID  map[string]*Event
}

// Open creates the store at path (a file path; rotation changes the
// extension's month part) and replays existing files from the current and
// previous month to rebuild the lookup index.
func Open(path string) (*Store, error) {
	dir, base := filepath.Split(path)
	if base == "" {
		return nil, fmt.Errorf("store: log path must be a file, got %q", path)
	}
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	s := &Store{dir: dir, base: trimExt(base), byID: map[string]*Event{}}
	now := time.Now()
	for _, m := range []time.Time{now.AddDate(0, -1, 0), now} {
		if err := s.replay(m.Format("2006-01")); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func trimExt(name string) string {
	if ext := filepath.Ext(name); ext != "" {
		return name[:len(name)-len(ext)]
	}
	return name
}

func (s *Store) fileFor(month string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s-%s.jsonl", s.base, month))
}

// replay loads one month's file into the index. Corrupt trailing lines (a
// crash mid-write) are skipped.
func (s *Store) replay(month string) error {
	f, err := os.Open(s.fileFor(month))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ResponseID != "" {
			s.byID[e.ResponseID] = &e
		}
	}
	return sc.Err()
}

// Append writes the event to the current month's file and indexes it.
func (s *Store) Append(e *Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	month := e.TS.Format("2006-01")
	if s.f == nil || month != s.month {
		if err := s.rotate(month); err != nil {
			return err
		}
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if _, err := s.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	s.byID[e.ResponseID] = e
	return nil
}

func (s *Store) rotate(month string) error {
	if s.f != nil {
		s.w.Flush()
		s.f.Close()
	}
	f, err := os.OpenFile(s.fileFor(month), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	s.f = f
	s.w = bufio.NewWriter(f)
	s.month = month
	return nil
}

// Get returns a copy of the latest event for the response id.
func (s *Store) Get(responseID string) (*Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.byID[responseID]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// Close flushes and closes the open file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	s.w.Flush()
	err := s.f.Close()
	s.f = nil
	return err
}
