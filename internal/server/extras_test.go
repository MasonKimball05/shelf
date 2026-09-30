package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
	"github.com/MasonKimball05/shelf/internal/progress"
	"github.com/MasonKimball05/shelf/internal/thumbs"
)

// setupExtras is setup with saved positions on, and thumbnails on when ffmpeg
// is installed (withVideo makes Clip.mp4 a real video so ffmpeg can read it).
func setupExtras(t *testing.T, withVideo bool) (http.Handler, *library.Library) {
	t.Helper()
	dir := t.TempDir()
	movies := filepath.Join(dir, "Movies")
	os.MkdirAll(movies, 0o755)
	clip := filepath.Join(movies, "Clip.mp4")
	if withVideo {
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			t.Skip("ffmpeg not installed")
		}
		cmd := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=3", "-y", clip)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	} else {
		os.WriteFile(clip, []byte("0123456789"), 0o644)
	}
	os.WriteFile(filepath.Join(movies, "Song.mp3"), []byte("id3"), 0o644)
	lib := library.New([]library.Root{{Name: "Movies", Path: movies}})
	lib.Scan()

	s := New(lib, NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour), token, "http://desktop:8095")
	store, err := progress.Open(filepath.Join(dir, "data", "progress.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.SetProgress(store)
	if maker, err := thumbs.New(filepath.Join(dir, "data", "thumbs"), ""); err == nil {
		s.SetThumbnails(maker)
	}
	return s.Handler(), lib
}

func post(t *testing.T, h http.Handler, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestNewEndpointsRequireToken(t *testing.T) {
	h, _ := setupExtras(t, false)
	id := library.ID("Movies", "Clip.mp4")
	if rec := do(t, h, "GET", "/api/thumb?id="+id, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("thumb without token: %d", rec.Code)
	}
	if rec := post(t, h, "/api/progress", `{"id":"`+id+`","position":60}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("progress without token: %d", rec.Code)
	}
}

func TestProgressRoundTrip(t *testing.T) {
	h, _ := setupExtras(t, false)
	id := library.ID("Movies", "Clip.mp4")
	if rec := post(t, h, "/api/progress", `{"id":"`+id+`","position":754.5,"duration":5400}`, authed); rec.Code != http.StatusNoContent {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}

	// Browse shows it on the file...
	rec := do(t, h, "GET", "/api/browse?root=Movies&path=", authed)
	var listing struct {
		Files []struct {
			ID       string  `json:"id"`
			Position float64 `json:"position"`
			Duration float64 `json:"duration"`
		} `json:"files"`
	}
	json.NewDecoder(rec.Body).Decode(&listing)
	found := false
	for _, f := range listing.Files {
		if f.ID == id {
			found = f.Position == 754.5 && f.Duration == 5400
		}
	}
	if !found {
		t.Errorf("browse didn't include the position: %+v", listing.Files)
	}

	// ...and the link, which is what a player resumes from.
	rec = do(t, h, "POST", "/api/link?id="+id, authed)
	var l link
	json.NewDecoder(rec.Body).Decode(&l)
	if l.Position != 754.5 {
		t.Errorf("link position = %v, want 754.5", l.Position)
	}

	// Finishing the file clears it.
	post(t, h, "/api/progress", `{"id":"`+id+`","position":5398,"duration":5400}`, authed)
	rec = do(t, h, "POST", "/api/link?id="+id, authed)
	l = link{}
	json.NewDecoder(rec.Body).Decode(&l)
	if l.Position != 0 {
		t.Errorf("finished file still resumes at %v", l.Position)
	}
}

func TestProgressValidation(t *testing.T) {
	h, _ := setupExtras(t, false)
	id := library.ID("Movies", "Clip.mp4")
	cases := map[string]struct {
		body string
		want int
	}{
		"not json":        {`position=5`, http.StatusBadRequest},
		"negative":        {`{"id":"` + id + `","position":-5}`, http.StatusBadRequest},
		"absurdly large":  {`{"id":"` + id + `","position":1e300}`, http.StatusBadRequest},
		"unknown file":    {`{"id":"deadbeef","position":60}`, http.StatusNotFound},
		"oversized body":  {`{"id":"` + strings.Repeat("a", 8<<10) + `"}`, http.StatusBadRequest},
		"valid is stored": {`{"id":"` + id + `","position":60}`, http.StatusNoContent},
	}
	for name, c := range cases {
		if rec := post(t, h, "/api/progress", c.body, authed); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", name, rec.Code, c.want, rec.Body)
		}
	}
}

func TestThumbnail(t *testing.T) {
	h, _ := setupExtras(t, true)
	rec := do(t, h, "GET", "/api/thumb?id="+library.ID("Movies", "Clip.mp4"), authed)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if b := rec.Body.Bytes(); len(b) < 2 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Error("body isn't a JPEG")
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("Cache-Control %q, want private caching", cc)
	}
	// Audio has no poster frame; unknown IDs aren't files.
	for _, id := range []string{library.ID("Movies", "Song.mp3"), "deadbeef"} {
		if rec := do(t, h, "GET", "/api/thumb?id="+id, authed); rec.Code != http.StatusNotFound {
			t.Errorf("id %s: got %d, want 404", id, rec.Code)
		}
	}
}

func TestThumbnailsOffWithoutFFmpeg(t *testing.T) {
	h, _, _ := setup(t) // the plain setup never turns thumbnails on
	if rec := do(t, h, "GET", "/api/thumb?id="+library.ID("Movies", "Clip.mp4"), authed); rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
}
