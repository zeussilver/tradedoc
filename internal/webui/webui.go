// Package webui serves the React frontend. Vite builds it into dist/ (see
// web/vite.config.ts) and the files are embedded into the Go binary, so one
// executable (or one Lambda) serves both the API and the UI.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// "all:" also embeds dist/.gitkeep, so this compiles on a fresh clone where
// the frontend has never been built (dist/ would otherwise be empty, and
// //go:embed refuses an empty directory).
//
//go:embed all:dist
var dist embed.FS

// notBuilt is served instead of the app when dist/index.html is missing.
const notBuilt = "TradeDoc Check: the frontend isn't built.\n\n" +
	"Run `make web` (or `cd web && npm ci && npm run build`), then rebuild and restart the server.\n" +
	"The API under /v1 works without it.\n"

// Handler serves the embedded frontend build.
func Handler() http.Handler {
	files, err := fs.Sub(dist, "dist")
	if err != nil {
		// Cannot happen: "dist" is checked by the compiler via go:embed.
		panic(err)
	}
	return newHandler(files)
}

// newHandler takes the files as an fs.FS so tests can pass an in-memory
// fstest.MapFS instead of depending on a real Vite build.
func newHandler(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// path.Clean removes "..", so a request can't climb out of dist/.
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")

		if name != "" && isFile(files, name) {
			// Vite puts a content hash in every file name under assets/, so a
			// changed file gets a new URL and these can be cached for a year.
			// Anything else (index.html, sample PDFs) is checked every time.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFileFS(w, r, files, name)
			return
		}

		// A path that looks like a file (has an extension) but doesn't exist
		// is a real 404: answering a missing .js file with index.html would
		// only produce a confusing error in the browser.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}

		// Anything else is a page of the single-page app: serve index.html
		// and let the app decide what to show.
		if !isFile(files, "index.html") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, files, "index.html")
	})
}

// isFile reports whether name is a regular, non-hidden file. Directories
// are excluded so there is never a directory listing, and dotfiles (such as
// .gitkeep) are not part of the site.
func isFile(files fs.FS, name string) bool {
	if strings.HasPrefix(path.Base(name), ".") {
		return false
	}
	info, err := fs.Stat(files, name)
	return err == nil && info.Mode().IsRegular()
}
