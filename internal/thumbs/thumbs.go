// Package thumbs makes poster-frame thumbnails for videos with ffmpeg and
// caches them on disk.
//
// A thumbnail is the frame a tenth of the way in (past the black opening most
// videos have), scaled to 480px wide. The cache key includes the file's size
// and modification time, so replacing a file makes a new thumbnail. Failures
// are cached too, so a file ffmpeg can't read isn't retried on every request.
package thumbs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
)

// ErrUnavailable means there's no thumbnail for this file: it isn't a video,
// or ffmpeg couldn't get a frame out of it.
var ErrUnavailable = errors.New("no thumbnail")

// Maker is safe for concurrent use.
type Maker struct {
	ffmpeg  string
	ffprobe string // "" when not found: fall back to a fixed seek
	dir     string
	sem     chan struct{} // limits concurrent ffmpeg runs

	mu       sync.Mutex
	inflight map[string]*call
}

type call struct {
	done chan struct{}
	err  error
}

// New finds ffmpeg (ffmpegPath, or on PATH when empty) and creates the cache
// directory. It returns an error when ffmpeg isn't available; the server then
// runs without thumbnails.
func New(dir, ffmpegPath string) (*Maker, error) {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	ffmpeg, err := exec.LookPath(ffmpegPath)
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found: %w", err)
	}
	// ffprobe ships next to ffmpeg; use the one beside it, not whatever PATH finds.
	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe"+filepath.Ext(ffmpeg))
	if _, err := os.Stat(ffprobe); err != nil {
		ffprobe = ""
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Maker{ffmpeg: ffmpeg, ffprobe: ffprobe, dir: dir, sem: make(chan struct{}, 2), inflight: map[string]*call{}}, nil
}

func key(f library.File) string {
	return fmt.Sprintf("%s-%x-%x", f.ID, f.Size, f.Modified.Unix())
}

// Get returns the path of the cached JPEG for a video, making it first if
// needed. abs is the file's location on disk, from the library index.
func (m *Maker) Get(ctx context.Context, f library.File, abs string) (string, error) {
	if f.Kind != library.Video {
		return "", ErrUnavailable
	}
	k := key(f)
	jpg := filepath.Join(m.dir, k+".jpg")
	failed := filepath.Join(m.dir, k+".none")
	if _, err := os.Stat(jpg); err == nil {
		return jpg, nil
	}
	if _, err := os.Stat(failed); err == nil {
		return "", ErrUnavailable
	}

	// Several rows of the same folder can ask at once; make each thumbnail once.
	m.mu.Lock()
	c, running := m.inflight[k]
	if !running {
		c = &call{done: make(chan struct{})}
		m.inflight[k] = c
		go m.make(c, k, abs, jpg, failed)
	}
	m.mu.Unlock()

	select {
	case <-c.done:
		if c.err != nil {
			return "", c.err
		}
		return jpg, nil
	case <-ctx.Done():
		// The generation keeps going, so the next request finds it cached.
		return "", ctx.Err()
	}
}

func (m *Maker) make(c *call, k, abs, jpg, failed string) {
	defer func() {
		m.mu.Lock()
		delete(m.inflight, k)
		m.mu.Unlock()
		close(c.done)
	}()
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	seek := 60.0
	if d, ok := m.duration(ctx, abs); ok {
		seek = d / 10
	}
	tmp := jpg + ".part"
	err := m.frame(ctx, abs, seek, tmp)
	if err != nil && seek > 0 {
		err = m.frame(ctx, abs, 0, tmp) // shorter than the guess, or a bad seek point
	}
	if err == nil {
		err = os.Rename(tmp, jpg)
	}
	if err != nil {
		_ = os.Remove(tmp)
		_ = os.WriteFile(failed, nil, 0o644)
		c.err = ErrUnavailable
	}
}

// Input paths get the "file:" prefix so ffmpeg never reads a name as another
// protocol (concat:, http:, subfile,...). They come from the index, not requests.
func (m *Maker) frame(ctx context.Context, abs string, seek float64, out string) error {
	cmd := exec.CommandContext(ctx, m.ffmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", strconv.FormatFloat(seek, 'f', 2, 64),
		"-i", "file:"+abs,
		"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "5",
		"-f", "mjpeg", "-y", "file:"+out)
	hideWindow(cmd)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		return errors.New("ffmpeg wrote no frame")
	}
	return nil
}

func (m *Maker) duration(ctx context.Context, abs string) (float64, bool) {
	if m.ffprobe == "" {
		return 0, false
	}
	cmd := exec.CommandContext(ctx, m.ffprobe, "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", "file:"+abs)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return d, err == nil && d > 0
}
