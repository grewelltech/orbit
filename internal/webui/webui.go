// Package webui embeds the built monitoring dashboard and serves it from the
// same mux as the Connect API (internal/server). Serving the UI in-process
// keeps ORBIT a single binary and avoids a cross-origin hop between the
// dashboard and the API it streams from.
//
// The dist directory is committed so `go build` and `go install` work without
// a Node toolchain; `make ui` regenerates it from web/.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the Vite build output. The all: prefix keeps files whose names
// begin with "_" or "." — Vite emits some — which plain //go:embed would skip.
//
//go:embed all:dist
var dist embed.FS

// indexPath is served for any route the asset tree doesn't contain, so
// client-side routes survive a page reload.
const indexPath = "index.html"

// Available reports whether a real UI build is embedded. A placeholder dist
// containing only .gitkeep yields false, which lets `orbit serve` start and
// explain the situation instead of serving a broken page.
func Available() bool {
	_, err := fs.Stat(assets(), indexPath)
	return err == nil
}

func assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the embed directive and this path disagree, which
		// is a build-time mistake rather than a runtime condition.
		panic("webui: dist subtree missing: " + err.Error())
	}
	return sub
}

// Handler serves the dashboard. Unknown paths fall back to index.html (SPA
// routing); missing builds yield a plain-text explanation rather than a 404
// that looks like a routing bug.
func Handler() http.Handler {
	files := assets()

	if !Available() {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("orbit: dashboard assets not built — run `make ui`\n"))
		})
	}

	fileServer := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = indexPath
		}

		if _, err := fs.Stat(files, name); err != nil {
			// Unknown path: hand the SPA its entry point and let the client
			// router decide whether the route is real.
			serveIndex(w, r, files)
			return
		}

		// Vite fingerprints asset filenames, so they are safe to cache
		// indefinitely. index.html must not be, or clients pin to a stale build.
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}

		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, files fs.FS) {
	data, err := fs.ReadFile(files, indexPath)
	if err != nil {
		http.Error(w, "dashboard unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// ServeContent would need a ReadSeeker and an mtime the embed FS lacks;
	// the index is small enough that a direct write is simpler and adequate.
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}
