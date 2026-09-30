// Package server is shelf's HTTP API and streaming endpoint.
//
//	GET  /api/roots                    library roots          (token required)
//	GET  /api/browse?root=&path=       one folder             (token required)
//	GET  /api/search?q=                search by name/path    (token required)
//	POST /api/link?id=                 a signed stream link   (token required)
//	POST /api/rescan                   rescan the folders     (token required)
//	GET  /api/thumb?id=                a video's poster frame (token required)
//	POST /api/progress                 save a resume position (token required)
//	GET  /stream/{id}/{name}?exp&sig   the file, with seeking (signature required)
//	GET  /healthz                      for homebase
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
	"github.com/MasonKimball05/shelf/internal/progress"
	"github.com/MasonKimball05/shelf/internal/thumbs"
)

type Server struct {
	lib    *library.Library
	signer *Signer
	token  string // API bearer token
	public string // base URL clients use, e.g. http://arkans-pc1:8095

	thumbs   *thumbs.Maker   // nil when ffmpeg isn't installed
	progress *progress.Store // nil disables resume-across-devices
}

func New(lib *library.Library, signer *Signer, token, publicURL string) *Server {
	return &Server{lib: lib, signer: signer, token: token, public: strings.TrimRight(publicURL, "/")}
}

// SetThumbnails turns on /api/thumb.
func (s *Server) SetThumbnails(m *thumbs.Maker) { s.thumbs = m }

// SetProgress turns on saved positions: /api/progress, and a "position" on
// files in browse, search and link responses.
func (s *Server) SetProgress(p *progress.Store) { s.progress = p }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// A friendly answer for a browser pointed at the bare address. It's
	// unauthenticated, so it says nothing about the library itself.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("shelf is running.\nOpen the Library tab in Media Player to browse and play.\n"))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /api/roots", s.auth(s.handleRoots))
	mux.Handle("GET /api/browse", s.auth(s.handleBrowse))
	mux.Handle("GET /api/search", s.auth(s.handleSearch))
	mux.Handle("POST /api/link", s.auth(s.handleLink))
	mux.Handle("POST /api/rescan", s.auth(s.handleRescan))
	mux.Handle("GET /api/thumb", s.auth(s.handleThumb))
	mux.Handle("POST /api/progress", s.auth(s.handleProgress))
	mux.HandleFunc("GET /stream/{id}/{name}", s.handleStream)
	return headers(mux)
}

// auth requires "Authorization: Bearer <token>", compared in constant time.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong token"})
			return
		}
		next(w, r)
	})
}

func (s *Server) handleRoots(w http.ResponseWriter, r *http.Request) {
	files, scanned := s.lib.Stats()
	writeJSON(w, http.StatusOK, map[string]any{"roots": s.lib.Roots(), "files": files, "scanned_at": scanned})
}

// fileOut is a library file plus where playback last stopped, if anywhere.
type fileOut struct {
	library.File
	Position float64 `json:"position,omitempty"`
	Duration float64 `json:"duration,omitempty"`
}

func (s *Server) withProgress(files []library.File) []fileOut {
	out := make([]fileOut, len(files))
	for i, f := range files {
		out[i] = fileOut{File: f}
		if s.progress != nil {
			if e, ok := s.progress.Get(f.ID); ok {
				out[i].Position, out[i].Duration = e.Position, e.Duration
			}
		}
	}
	return out
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	listing, ok := s.lib.Browse(r.URL.Query().Get("root"), r.URL.Query().Get("path"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such library"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root": listing.Root, "path": listing.Path, "dirs": listing.Dirs, "files": s.withProgress(listing.Files),
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		q = q[:200]
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": s.withProgress(s.lib.Search(q, 200))})
}

type link struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Expires   int64  `json:"expires"`
	Subtitles []link `json:"subtitles,omitempty"`
	// Position is where playback last stopped, on any device.
	Position float64 `json:"position,omitempty"`
}

func (s *Server) streamLink(f library.File) link {
	exp, sig := s.signer.Sign(f.ID)
	u := s.public + "/stream/" + f.ID + "/" + url.PathEscape(f.Name) +
		"?exp=" + url.QueryEscape(itoa(exp)) + "&sig=" + url.QueryEscape(sig)
	return link{URL: u, Name: f.Name, Size: f.Size, Expires: exp}
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	f, ok := s.lib.Get(r.URL.Query().Get("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such file"})
		return
	}
	out := s.streamLink(f)
	if s.progress != nil {
		if e, ok := s.progress.Get(f.ID); ok {
			out.Position = e.Position
		}
	}
	for _, subID := range f.Subtitles {
		if sub, ok := s.lib.Get(subID); ok {
			out.Subtitles = append(out.Subtitles, s.streamLink(sub))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	if s.thumbs == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "thumbnails need ffmpeg on the server"})
		return
	}
	f, abs, ok := s.lib.Path(r.URL.Query().Get("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such file"})
		return
	}
	jpg, err := s.thumbs.Get(r.Context(), f, abs)
	switch {
	case errors.Is(err, thumbs.ErrUnavailable):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no thumbnail for this file"})
		return
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return // the client went away
	case err != nil:
		log.Printf("thumbnail %s: %v", f.Rel, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "couldn't make a thumbnail"})
		return
	}
	// A thumbnail only changes with its file, which changes its ID's cache key,
	// so the client may keep it. "private": it's still behind the token.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, jpg)
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	if s.progress == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "progress isn't enabled"})
		return
	}
	var in struct {
		ID       string  `json:"id"`
		Position float64 `json:"position"`
		Duration float64 `json:"duration"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected {id, position, duration}"})
		return
	}
	// JSON can't carry NaN or Inf, but a very large number is still possible.
	valid := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1e7 }
	if !valid(in.Position) || !valid(in.Duration) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "position and duration must be seconds"})
		return
	}
	// Only files in the library, so the store can't be filled with junk IDs.
	if _, ok := s.lib.Get(in.ID); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such file"})
		return
	}
	if err := s.progress.Set(in.ID, in.Position, in.Duration); err != nil {
		log.Printf("saving progress: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "couldn't save"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	_ = s.lib.Scan()
	files, _ := s.lib.Stats()
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "took_ms": time.Since(start).Milliseconds()})
}

// handleStream serves a file by ID. The {name} segment is cosmetic (players use
// it to guess the format); only the signed ID decides what's served.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.signer.Verify(id, r.URL.Query().Get("exp"), r.URL.Query().Get("sig")) {
		http.Error(w, "link expired or invalid", http.StatusForbidden)
		return
	}
	fh, f, err := s.lib.Open(id)
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("open %s: %v", f.Rel, err)
		http.Error(w, "can't read file", http.StatusInternalServerError)
		return
	}
	defer fh.Close()

	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(f.Name))); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// ServeContent handles Range requests, so players can seek and resume.
	http.ServeContent(w, r, f.Name, f.Modified, fh)
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
