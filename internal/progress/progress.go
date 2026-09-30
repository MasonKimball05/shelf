// Package progress remembers where playback stopped in each file, so a movie
// started on one Mac picks up at the same spot on another. It's a small JSON
// file on disk, rewritten atomically on every change.
package progress

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one file's saved position, in seconds.
type Entry struct {
	Position float64   `json:"position"`
	Duration float64   `json:"duration,omitempty"`
	Updated  time.Time `json:"updated"`
}

// edge is the "barely started" / "basically finished" cutoff, in seconds. It
// matches Media Player's own per-file resume (recordFileResumePosition), so a
// file drops out of both lists at the same moment.
const edge = 5

// Store is safe for concurrent use.
type Store struct {
	path string
	max  int // entries kept; the least recently updated go first
	now  func() time.Time

	mu sync.Mutex
	m  map[string]Entry
}

// Open loads the store at path, or starts empty if the file doesn't exist. A
// file that can't be parsed is moved aside (path + ".corrupt") rather than
// failing startup; the error is returned so the caller can log it.
func Open(path string) (*Store, error) {
	s := &Store{path: path, max: 5000, now: time.Now, m: map[string]Entry{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s.m); err != nil {
		s.m = map[string]Entry{}
		_ = os.Rename(path, path+".corrupt")
		return s, err
	}
	return s, nil
}

// Get returns the saved position for a file ID.
func (s *Store) Get(id string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[id]
	return e, ok
}

// Set records a position. Near the end of the file it forgets the entry
// instead (the file was watched); in the first few seconds it changes nothing.
func (s *Store) Set(id string, position, duration float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case duration > 0 && duration-position < edge:
		if _, ok := s.m[id]; !ok {
			return nil
		}
		delete(s.m, id)
	case position > edge:
		s.m[id] = Entry{Position: position, Duration: duration, Updated: s.now()}
		s.evictLocked()
	default:
		return nil
	}
	return s.saveLocked()
}

func (s *Store) evictLocked() {
	for len(s.m) > s.max {
		var oldestID string
		var oldest time.Time
		for id, e := range s.m {
			if oldestID == "" || e.Updated.Before(oldest) {
				oldestID, oldest = id, e.Updated
			}
		}
		delete(s.m, oldestID)
	}
}

// saveLocked writes to a temporary file and renames it over the real one, so
// a crash or power cut mid-write can't leave a half-written store behind.
func (s *Store) saveLocked() error {
	data, err := json.Marshal(s.m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
