package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
)

const token = "test-token-0123456789abcdef"

func setup(t *testing.T) (http.Handler, *Signer, *library.Library) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Movies"), 0o755)
	os.WriteFile(filepath.Join(dir, "Movies", "Clip.mp4"), []byte("0123456789"), 0o644)
	os.WriteFile(filepath.Join(dir, "Movies", "Clip.srt"), []byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n"), 0o644)
	lib := library.New([]library.Root{{Name: "Movies", Path: filepath.Join(dir, "Movies")}})
	lib.Scan()
	signer := NewSigner([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	return New(lib, signer, token, "http://desktop:8095").Handler(), signer, lib
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var authed = map[string]string{"Authorization": "Bearer " + token}

func TestAPIRequiresToken(t *testing.T) {
	h, _, _ := setup(t)
	for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer wrong"}, {"Authorization": token}} {
		if rec := do(t, h, "GET", "/api/roots", hdr); rec.Code != http.StatusUnauthorized {
			t.Errorf("headers %v: got %d, want 401", hdr, rec.Code)
		}
	}
	if rec := do(t, h, "GET", "/api/roots", authed); rec.Code != 200 {
		t.Errorf("valid token: got %d", rec.Code)
	}
}

func getLink(t *testing.T, h http.Handler, lib *library.Library) link {
	t.Helper()
	id := library.ID("Movies", "Clip.mp4")
	rec := do(t, h, "POST", "/api/link?id="+id, authed)
	var l link
	if err := json.NewDecoder(rec.Body).Decode(&l); err != nil || rec.Code != 200 {
		t.Fatalf("link: %d %v", rec.Code, err)
	}
	return l
}

func TestLinkIncludesSubtitles(t *testing.T) {
	h, _, lib := setup(t)
	l := getLink(t, h, lib)
	if !strings.HasPrefix(l.URL, "http://desktop:8095/stream/") || len(l.Subtitles) != 1 {
		t.Errorf("got %+v", l)
	}
}

func TestStreamSupportsSeeking(t *testing.T) {
	h, _, lib := setup(t)
	l := getLink(t, h, lib)
	u, _ := url.Parse(l.URL)
	rec := do(t, h, "GET", u.RequestURI(), map[string]string{"Range": "bytes=2-5"})
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" {
		t.Errorf("got %d %q, want 206 \"2345\"", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("content-type %q", ct)
	}
}

func TestStreamRejectsForgedLinks(t *testing.T) {
	h, _, lib := setup(t)
	l := getLink(t, h, lib)
	u, _ := url.Parse(l.URL)
	q := u.Query()
	otherID := library.ID("Movies", "Clip.srt")

	cases := map[string]string{
		"no signature":    "/stream/" + library.ID("Movies", "Clip.mp4") + "/Clip.mp4",
		"tampered sig":    u.Path + "?exp=" + q.Get("exp") + "&sig=AAAA" + q.Get("sig")[4:],
		"extended expiry": u.Path + "?exp=9999999999&sig=" + q.Get("sig"),
		"swapped file":    "/stream/" + otherID + "/Clip.srt?" + u.RawQuery,
	}
	for name, target := range cases {
		if rec := do(t, h, "GET", target, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, rec.Code)
		}
	}
}

func TestExpiredLinkIsRejected(t *testing.T) {
	h, signer, lib := setup(t)
	l := getLink(t, h, lib)
	signer.now = func() time.Time { return time.Now().Add(2 * time.Hour) } // past the 1h TTL
	u, _ := url.Parse(l.URL)
	if rec := do(t, h, "GET", u.RequestURI(), nil); rec.Code != http.StatusForbidden {
		t.Errorf("expired link: got %d", rec.Code)
	}
}

// The {name} segment is cosmetic: changing it can't select a different file.
func TestNameSegmentCannotChooseTheFile(t *testing.T) {
	h, _, lib := setup(t)
	l := getLink(t, h, lib)
	u, _ := url.Parse(l.URL)
	target := strings.Replace(u.RequestURI(), "/Clip.mp4?", "/..%2F..%2Fetc%2Fpasswd?", 1)
	rec := do(t, h, "GET", target, nil)
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 200 || string(body) != "0123456789" {
		t.Errorf("got %d %q: the signed ID alone should decide the file", rec.Code, body)
	}
}

func TestRootSaysRunningWithoutLeaking(t *testing.T) {
	h, _, _ := setup(t)
	rec := do(t, h, "GET", "/", nil)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "shelf is running") {
		t.Fatalf("GET /: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(t, h, "GET", "/nope", nil); rec.Code != 404 {
		t.Errorf("GET /nope: got %d, want 404 (only the bare root answers)", rec.Code)
	}
}
