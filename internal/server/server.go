// Package server is shelf's HTTP API and streaming endpoint.
//
//	GET  /api/roots                    library roots          (token required)
//	GET  /api/browse?root=&path=       one folder             (token required)
//	GET  /api/search?q=                search by name/path    (token required)
//	POST /api/link?id=                 a signed stream link   (token required)
//	POST /api/rescan                   rescan the folders     (token required)
//	GET  /stream/{id}/{name}?exp&sig   the file, with seeking (signature required)
//	GET  /healthz                      for homebase
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
)

type Server struct {
	lib    *library.Library
	signer *Signer
	token  string // API bearer token
	public string // base URL clients use, e.g. http://arkans-pc1:8095
}

func New(lib *library.Library, signer *Signer, token, publicURL string) *Server {
	return &Server{lib: lib, signer: signer, token: token, public: strings.TrimRight(publicURL, "/")}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /api/roots", s.auth(s.handleRoots))
	mux.Handle("GET /api/browse", s.auth(s.handleBrowse))
	mux.Handle("GET /api/search", s.auth(s.handleSearch))
	mux.Handle("POST /api/link", s.auth(s.handleLink))
	mux.Handle("POST /api/rescan", s.auth(s.handleRescan))
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

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	listing, ok := s.lib.Browse(r.URL.Query().Get("root"), r.URL.Query().Get("path"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such library"})
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		q = q[:200]
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": s.lib.Search(q, 200)})
}

type link struct {
	URL       string `json:"url"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Expires   int64  `json:"expires"`
	Subtitles []link `json:"subtitles,omitempty"`
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
	for _, subID := range f.Subtitles {
		if sub, ok := s.lib.Get(subID); ok {
			out.Subtitles = append(out.Subtitles, s.streamLink(sub))
		}
	}
	writeJSON(w, http.StatusOK, out)
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
