package thumbs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
)

// video makes a short test clip with ffmpeg, or skips the test without it.
func video(t *testing.T, seconds string) (string, library.File) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "Clip.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration="+seconds, "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("making test video: %v %s", err, out)
	}
	st, _ := os.Stat(path)
	return path, library.File{ID: "abc", Name: "Clip.mp4", Kind: library.Video, Size: st.Size(), Modified: st.ModTime()}
}

func newMaker(t *testing.T) *Maker {
	t.Helper()
	m, err := New(t.TempDir(), "")
	if err != nil {
		t.Skip(err)
	}
	return m
}

func TestMakesAndCachesThumbnail(t *testing.T) {
	path, f := video(t, "3")
	m := newMaker(t)
	jpg, err := m.Get(context.Background(), f, path)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(jpg)
	if len(data) < 3 || data[0] != 0xFF || data[1] != 0xD8 {
		t.Fatalf("not a JPEG (%d bytes)", len(data))
	}
	// Cached: a second call returns the same file without running ffmpeg.
	m.ffmpeg = "/nonexistent/ffmpeg"
	again, err := m.Get(context.Background(), f, path)
	if err != nil || again != jpg {
		t.Errorf("second call: %q, %v", again, err)
	}
}

func TestChangedFileGetsNewThumbnail(t *testing.T) {
	path, f := video(t, "3")
	m := newMaker(t)
	first, _ := m.Get(context.Background(), f, path)
	f.Modified = f.Modified.Add(time.Hour)
	second, err := m.Get(context.Background(), f, path)
	if err != nil || second == first {
		t.Errorf("replaced file reused the old thumbnail (%q, %v)", second, err)
	}
}

func TestNotAVideo(t *testing.T) {
	m := newMaker(t)
	f := library.File{ID: "x", Kind: library.Audio}
	if _, err := m.Get(context.Background(), f, "/whatever.mp3"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("got %v, want ErrUnavailable", err)
	}
}

func TestUnreadableFileFailsOnceAndIsRemembered(t *testing.T) {
	m := newMaker(t)
	bad := filepath.Join(t.TempDir(), "Broken.mkv")
	os.WriteFile(bad, []byte("not a video"), 0o644)
	f := library.File{ID: "bad", Kind: library.Video, Size: 11, Modified: time.Now()}
	if _, err := m.Get(context.Background(), f, bad); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v, want ErrUnavailable", err)
	}
	m.ffmpeg = "/nonexistent/ffmpeg" // a retry would fail differently
	if _, err := m.Get(context.Background(), f, bad); !errors.Is(err, ErrUnavailable) {
		t.Errorf("second call: got %v, want the cached failure", err)
	}
}

func TestConcurrentRequestsShareOneRun(t *testing.T) {
	path, f := video(t, "3")
	m := newMaker(t)
	var wg sync.WaitGroup
	paths := make([]string, 6)
	for i := range paths {
		wg.Go(func() {
			paths[i], _ = m.Get(context.Background(), f, path)
		})
	}
	wg.Wait()
	for _, p := range paths {
		if p == "" || p != paths[0] {
			t.Fatalf("got %v, want the same thumbnail for every caller", paths)
		}
	}
	entries, _ := os.ReadDir(m.dir)
	if len(entries) != 1 {
		t.Errorf("cache has %d files, want 1", len(entries))
	}
}
