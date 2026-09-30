// Package library scans media folders into an in-memory index.
//
// Every file gets an opaque ID derived from its root and relative path. The
// server only ever streams by ID, never by a path from the request, so path
// traversal ("../../Windows") is impossible by construction.
package library

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Kind is what a file is for.
type Kind string

const (
	Video    Kind = "video"
	Audio    Kind = "audio"
	Subtitle Kind = "subtitle"
)

var kinds = map[string]Kind{
	".mkv": Video, ".mp4": Video, ".m4v": Video, ".mov": Video, ".avi": Video,
	".webm": Video, ".wmv": Video, ".ts": Video, ".m2ts": Video, ".flv": Video, ".mpg": Video, ".mpeg": Video,
	".mp3": Audio, ".flac": Audio, ".m4a": Audio, ".aac": Audio, ".ogg": Audio, ".opus": Audio,
	".wav": Audio, ".alac": Audio, ".aiff": Audio, ".wma": Audio,
	".srt": Subtitle, ".ass": Subtitle, ".ssa": Subtitle, ".vtt": Subtitle, ".sub": Subtitle,
}

// KindOf returns the kind for a filename, or "" if it isn't media.
func KindOf(name string) Kind { return kinds[strings.ToLower(filepath.Ext(name))] }

// Root is a configured top-level folder, e.g. {Name: "Movies", Path: `D:\Movies`}.
type Root struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// File is one indexed media file. Rel uses forward slashes on every OS.
type File struct {
	ID       string    `json:"id"`
	Root     string    `json:"root"`
	Rel      string    `json:"path"`
	Name     string    `json:"name"`
	Kind     Kind      `json:"kind"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	// Subtitles are the IDs of subtitle files next to a video that share its
	// base name (Movie.mkv -> Movie.srt, Movie.en.srt).
	Subtitles []string `json:"subtitles,omitempty"`

	abs string // absolute path on disk; never sent to clients
}

// Library is the index. Safe for concurrent use; Scan swaps it atomically.
type Library struct {
	roots []Root

	mu      sync.RWMutex
	byID    map[string]*File
	byDir   map[string][]*File // key: root + "/" + dir
	dirs    map[string][]string
	scanned time.Time
}

// New creates an empty library over the given roots. Call Scan to fill it.
func New(roots []Root) *Library {
	return &Library{roots: roots, byID: map[string]*File{}, byDir: map[string][]*File{}, dirs: map[string][]string{}}
}

// ID derives a file's stable, opaque ID.
func ID(root, rel string) string {
	sum := sha256.Sum256([]byte(root + "\x00" + rel))
	return hex.EncodeToString(sum[:12])
}

// Scan walks every root and replaces the index. Unreadable folders are skipped.
func (l *Library) Scan() error {
	byID := map[string]*File{}
	byDir := map[string][]*File{}
	dirs := map[string][]string{}

	for _, root := range l.roots {
		base, err := filepath.Abs(root.Path)
		if err != nil {
			continue
		}
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // keep going past unreadable entries
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.EqualFold(name, "$RECYCLE.BIN") || strings.EqualFold(name, "System Volume Information") {
				if d.IsDir() && p != base {
					return filepath.SkipDir
				}
				return nil
			}
			// Don't follow symlinks or junctions: they could point outside the root.
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			rel, err := filepath.Rel(base, p)
			if err != nil || rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)
			parent := path.Dir(rel)
			if parent == "." {
				parent = ""
			}
			if d.IsDir() {
				key := root.Name + "/" + parent
				dirs[key] = append(dirs[key], name)
				return nil
			}
			kind := KindOf(name)
			if kind == "" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			f := &File{
				ID: ID(root.Name, rel), Root: root.Name, Rel: rel, Name: name, Kind: kind,
				Size: info.Size(), Modified: info.ModTime().UTC().Truncate(time.Second), abs: p,
			}
			if _, dup := byID[f.ID]; !dup {
				byID[f.ID] = f
			}
			key := root.Name + "/" + parent
			byDir[key] = append(byDir[key], f)
			return nil
		})
	}

	// Several folders can share a root name (e.g. Movies on two drives) and show
	// as one. A file at the same relative path in both gets the same ID; keep
	// the first copy, and list shared subfolder names once.
	for key, files := range byDir {
		seen := map[string]bool{}
		kept := files[:0]
		for _, f := range files {
			if !seen[f.ID] {
				seen[f.ID] = true
				kept = append(kept, f)
			}
		}
		byDir[key] = kept
	}
	for key, names := range dirs {
		slices.Sort(names)
		dirs[key] = slices.Compact(names)
	}

	linkSubtitles(byDir)
	for _, files := range byDir {
		slices.SortFunc(files, func(a, b *File) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	}
	for k := range dirs {
		slices.SortFunc(dirs[k], func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	}

	l.mu.Lock()
	l.byID, l.byDir, l.dirs, l.scanned = byID, byDir, dirs, time.Now().UTC()
	l.mu.Unlock()
	return nil
}

// linkSubtitles attaches Movie.srt / Movie.en.srt to Movie.mkv in the same folder.
func linkSubtitles(byDir map[string][]*File) {
	for _, files := range byDir {
		for _, v := range files {
			if v.Kind != Video {
				continue
			}
			stem := strings.ToLower(strings.TrimSuffix(v.Name, filepath.Ext(v.Name)))
			for _, s := range files {
				if s.Kind != Subtitle {
					continue
				}
				sub := strings.ToLower(strings.TrimSuffix(s.Name, filepath.Ext(s.Name)))
				if sub == stem || strings.HasPrefix(sub, stem+".") {
					v.Subtitles = append(v.Subtitles, s.ID)
				}
			}
		}
	}
}

// Roots lists root names in config order, once each (a name can cover several folders).
func (l *Library) Roots() []string {
	var out []string
	for _, r := range l.roots {
		if !slices.Contains(out, r.Name) {
			out = append(out, r.Name)
		}
	}
	return out
}

// Listing is one folder's contents.
type Listing struct {
	Root  string   `json:"root"`
	Path  string   `json:"path"`
	Dirs  []string `json:"dirs"`
	Files []File   `json:"files"` // subtitles are omitted: they ride along with their video
}

// Browse lists one folder. dir is relative to the root with forward slashes;
// it is only ever used as a map key, never touched on disk.
func (l *Library) Browse(root, dir string) (Listing, bool) {
	if !slices.Contains(l.Roots(), root) {
		return Listing{}, false
	}
	dir = strings.Trim(path.Clean("/"+dir), "/")
	key := root + "/" + dir
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := Listing{Root: root, Path: dir, Dirs: append([]string{}, l.dirs[key]...), Files: []File{}}
	for _, f := range l.byDir[key] {
		if f.Kind != Subtitle {
			out.Files = append(out.Files, *f)
		}
	}
	return out, true
}

// Search finds media whose path contains every word of q (case-insensitive).
func (l *Library) Search(q string, limit int) []File {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return []File{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := []File{}
	for _, f := range l.byID {
		if f.Kind == Subtitle {
			continue
		}
		hay := strings.ToLower(f.Root + "/" + f.Rel)
		match := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				match = false
				break
			}
		}
		if match {
			out = append(out, *f)
		}
	}
	slices.SortFunc(out, func(a, b File) int { return strings.Compare(strings.ToLower(a.Rel), strings.ToLower(b.Rel)) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Get returns a file by ID.
func (l *Library) Get(id string) (File, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	f, ok := l.byID[id]
	if !ok {
		return File{}, false
	}
	return *f, true
}

// Open opens a file by ID for streaming.
func (l *Library) Open(id string) (*os.File, File, error) {
	f, ok := l.Get(id)
	if !ok {
		return nil, File{}, fs.ErrNotExist
	}
	fh, err := os.Open(f.abs)
	return fh, f, err
}

// Stats summarizes the index.
func (l *Library) Stats() (files int, scanned time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.byID), l.scanned
}
