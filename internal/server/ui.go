package server

import (
	"io/fs"
	"net/http"
	"strings"
)

// uiFS holds the UI build served under /ui/. Its value depends on build tags:
// embed.go embeds the real ui_dist build when compiled with `-tags embed_ui`,
// while embed_notag.go provides an empty filesystem for plain builds and tests.
// See those files for details.

// acceptsBrotli reports whether the client accepts brotli. Brotli is the only
// encoding the UI build pre-compresses; clients without it get the original
// file uncompressed.
func acceptsBrotli(acceptEncoding string) bool {
	for _, part := range strings.Split(acceptEncoding, ",") {
		if strings.TrimSpace(strings.SplitN(part, ";", 2)[0]) == "br" {
			return true
		}
	}
	return false
}

// serveCompressedFile serves name from fsys, preferring a pre-compressed
// sibling (name+".br") when the client accepts brotli. It returns an error
// without writing a response when name cannot be served, so callers can fall
// back (e.g. SPA routing).
func serveCompressedFile(fsys http.FileSystem, w http.ResponseWriter, r *http.Request, name string) error {
	if acceptsBrotli(r.Header.Get("Accept-Encoding")) {
		if cf, err := fsys.Open(name + ".br"); err == nil {
			defer cf.Close()
			if stat, err := cf.Stat(); err == nil && !stat.IsDir() {
				w.Header().Set("Content-Encoding", "br")
				w.Header().Add("Vary", "Accept-Encoding")
				http.ServeContent(w, r, name, stat.ModTime(), cf)
				return nil
			}
		}
	}

	file, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.IsDir() {
		return fs.ErrNotExist
	}

	http.ServeContent(w, r, name, stat.ModTime(), file)
	return nil
}

// handleUI serves the embedded SPA under /ui/.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	serveUI(uiFS, w, r)
}

// serveUI serves the SPA from fsys. Real files are served with compression
// support.
//
// The UI routes with the URL hash (svelte-spa-router), so every navigation
// after the first load stays on /ui/ and only asset paths reach this handler.
// Consequently an unknown path is a genuine 404 rather than a request to
// return index.html: the old fallback made every guessed path under /ui/
// indistinguishable from a real one, which is both a path-disclosure oracle
// and pointless noise in access logs.
func serveUI(fsys http.FileSystem, w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" {
		name = "index.html"
	}
	// The entry document names every lazy-loaded chunk. Never let a browser
	// keep an older copy across a binary upgrade, otherwise its next route
	// navigation can request a chunk that no longer exists in the new embed.
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}

	if err := serveCompressedFile(fsys, w, r, name); err != nil {
		http.NotFound(w, r)
	}
}

// handleFavicon serves /favicon.ico from the embedded UI build.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if err := serveCompressedFile(uiFS, w, r, "favicon.ico"); err != nil {
		http.NotFound(w, r)
	}
}
