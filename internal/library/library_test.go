package library

import (
	"os"
	"path/filepath"
	"testing"
)

// makeTree builds a small fake library and returns its root.
func makeTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{
		"Movies/Heat (1995)/Heat.mkv",
		"Movies/Heat (1995)/Heat.en.srt",
		"Movies/Heat (1995)/Heat.nfo", // not media: ignored
		"Movies/Arrival.mp4",
		"Movies/.hidden/secret.mkv", // hidden folder: skipped
		"TV/Show/Season 1/S01E01.mkv",
		"Music/Album/01 Song.flac",
	} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data:"+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newLib(t *testing.T) (*Library, string) {
	dir := makeTree(t)
	l := New([]Root{{Name: "Movies", Path: filepath.Join(dir, "Movies")}, {Name: "TV", Path: filepath.Join(dir, "TV")}})
	if err := l.Scan(); err != nil {
		t.Fatal(err)
	}
	return l, dir
}

func TestScanAndBrowse(t *testing.T) {
	l, _ := newLib(t)

	top, ok := l.Browse("Movies", "")
	if !ok {
		t.Fatal("Movies root missing")
	}
	if len(top.Dirs) != 1 || top.Dirs[0] != "Heat (1995)" {
		t.Errorf("dirs = %v (hidden folder must be skipped)", top.Dirs)
	}
	if len(top.Files) != 1 || top.Files[0].Name != "Arrival.mp4" {
		t.Errorf("files = %+v", top.Files)
	}

	heat, _ := l.Browse("Movies", "Heat (1995)")
	if len(heat.Files) != 1 || heat.Files[0].Name != "Heat.mkv" {
		t.Fatalf("subtitles and non-media must not be listed: %+v", heat.Files)
	}
	if len(heat.Files[0].Subtitles) != 1 {
		t.Errorf("Heat.en.srt should be attached to Heat.mkv: %+v", heat.Files[0])
	}
}

func TestIDsAreStableAndOpaque(t *testing.T) {
	l, _ := newLib(t)
	f := l.Search("arrival", 10)[0]
	if f.ID != ID("Movies", "Arrival.mp4") || len(f.ID) != 24 {
		t.Errorf("id %q", f.ID)
	}
	fh, got, err := l.Open(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	fh.Close()
	if got.Rel != "Arrival.mp4" {
		t.Errorf("opened %q", got.Rel)
	}
	if _, _, err := l.Open("../../etc/passwd"); err == nil {
		t.Error("an arbitrary path must never open")
	}
}

// Browse only uses the path as a lookup key, so traversal can't leave the root.
func TestBrowseTraversalStaysInside(t *testing.T) {
	l, _ := newLib(t)
	for _, p := range []string{"../TV", "../../", "Heat (1995)/../../TV"} {
		got, _ := l.Browse("Movies", p)
		for _, f := range got.Files {
			if f.Root != "Movies" {
				t.Errorf("Browse(%q) escaped to %s/%s", p, f.Root, f.Rel)
			}
		}
	}
	if _, ok := l.Browse("Nope", ""); ok {
		t.Error("unknown root accepted")
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	dir := makeTree(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "private.mkv"), []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "Movies", "link")); err != nil {
		t.Skip("symlinks unsupported here:", err)
	}
	l := New([]Root{{Name: "Movies", Path: filepath.Join(dir, "Movies")}})
	l.Scan()
	if got := l.Search("private", 10); len(got) != 0 {
		t.Errorf("symlinked file outside the root was indexed: %+v", got)
	}
}

func TestSearchMatchesAllWords(t *testing.T) {
	l, _ := newLib(t)
	if got := l.Search("season s01e01", 10); len(got) != 1 {
		t.Errorf("got %+v", got)
	}
	if got := l.Search("heat arrival", 10); len(got) != 0 {
		t.Errorf("all words must match: %+v", got)
	}
	if got := l.Search("", 10); len(got) != 0 {
		t.Error("empty query should return nothing")
	}
}

func TestTwoFoldersCanShareARoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for dir, files := range map[string][]string{
		a: {"Heat.mkv", "Shared/One.mkv", "Dupe.mkv"},
		b: {"Arrival.mkv", "Shared/Two.mkv", "Dupe.mkv"},
	} {
		for _, f := range files {
			full := filepath.Join(dir, filepath.FromSlash(f))
			os.MkdirAll(filepath.Dir(full), 0o755)
			os.WriteFile(full, []byte(f), 0o644)
		}
	}
	l := New([]Root{{Name: "Movies", Path: a}, {Name: "Movies", Path: b}})
	l.Scan()

	if got := l.Roots(); len(got) != 1 || got[0] != "Movies" {
		t.Errorf("roots = %v, want one Movies", got)
	}
	top, _ := l.Browse("Movies", "")
	if len(top.Dirs) != 1 || top.Dirs[0] != "Shared" {
		t.Errorf("dirs = %v, want Shared once", top.Dirs)
	}
	if len(top.Files) != 3 { // Arrival, Dupe (once), Heat
		t.Errorf("files = %+v", top.Files)
	}
	shared, _ := l.Browse("Movies", "Shared")
	if len(shared.Files) != 2 {
		t.Errorf("Shared should merge both drives: %+v", shared.Files)
	}
}
