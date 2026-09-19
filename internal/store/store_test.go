package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAppendGetReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scores.jsonl")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w2 := 2.5
	e1 := &Event{TS: time.Now(), ResponseID: "a", RubricVersion: "v1", Status: "ok", Weighted: &w2}
	e2 := &Event{TS: time.Now(), ResponseID: "b", RubricVersion: "v1", Status: "skipped", Reason: "queue_full"}
	if err := s.Append(e1); err != nil {
		t.Fatalf("append e1: %v", err)
	}
	if err := s.Append(e2); err != nil {
		t.Fatalf("append e2: %v", err)
	}
	if got, ok := s.Get("a"); !ok || got.Status != "ok" || *got.Weighted != 2.5 {
		t.Fatalf("get a = %+v ok=%v", got, ok)
	}
	s.Close()

	// Reopen: the index must be rebuilt from the file on disk.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	for id, want := range map[string]string{"a": "ok", "b": "skipped"} {
		got, ok := s2.Get(id)
		if !ok || got.Status != want {
			t.Fatalf("replayed %s = %+v ok=%v, want status %s", id, got, ok, want)
		}
	}
	if _, ok := s2.Get("missing"); ok {
		t.Fatal("missing id returned an event")
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "s.jsonl"))
	defer s.Close()
	w := 1.0
	s.Append(&Event{TS: time.Now(), ResponseID: "x", RubricVersion: "v1", Status: "ok", Weighted: &w})
	got, _ := s.Get("x")
	got.Status = "mutated"
	again, _ := s.Get("x")
	if again.Status != "ok" {
		t.Fatalf("Get leaked internal state: %+v", again)
	}
}
