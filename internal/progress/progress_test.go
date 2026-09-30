package progress

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSetGetAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("a", 120, 3600); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := s2.Get("a")
	if !ok || e.Position != 120 || e.Duration != 3600 {
		t.Fatalf("after reload got %+v, %v", e, ok)
	}
}

func TestEdges(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "p.json"))
	s.Set("a", 3, 3600) // barely started: not saved
	if _, ok := s.Get("a"); ok {
		t.Error("position under 5s was saved")
	}
	s.Set("a", 600, 3600)
	s.Set("a", 3, 3600) // restarting from the top keeps the old spot until you pass 5s
	if e, _ := s.Get("a"); e.Position != 600 {
		t.Errorf("got %v, want 600 kept", e.Position)
	}
	s.Set("a", 3597, 3600) // basically finished: forgotten
	if _, ok := s.Get("a"); ok {
		t.Error("finished file still has a position")
	}
}

func TestEvictsOldest(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "p.json"))
	s.max = 2
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	s.Set("old", 60, 0)
	s.Set("mid", 60, 0)
	s.Set("new", 60, 0)
	if _, ok := s.Get("old"); ok {
		t.Error("oldest entry wasn't evicted")
	}
	if _, ok := s.Get("new"); !ok {
		t.Error("newest entry missing")
	}
}

func TestCorruptFileIsMovedAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	s, err := Open(path)
	if err == nil {
		t.Fatal("want an error for a corrupt file")
	}
	if _, statErr := os.Stat(path + ".corrupt"); statErr != nil {
		t.Errorf("corrupt file not kept: %v", statErr)
	}
	if err := s.Set("a", 60, 0); err != nil {
		t.Errorf("store unusable after corrupt load: %v", err)
	}
}
