package server

import (
	"embed"
	"io/fs"
	"net/http"
)

// uiFiles is the management UI. It is plain HTML/CSS/JS, embedded at build time:
// the binary still needs no Node runtime and no external asset directory.
//
//go:embed ui
var uiFiles embed.FS

// uiHandler serves the static UI when the admin API is enabled.
//
// The page itself carries no secrets, but serving it while the API is off would
// only show a dead dashboard, so the same switch gates both.
func (s *Server) uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic("embedded ui is missing: " + err.Error())
	}
	files := http.StripPrefix("/ui", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.Config()
		if !cfg.Admin.Enabled || s.Admin == nil {
			writeJSONError(w, http.StatusNotFound, "admin UI is disabled", "not_found")
			return
		}
		// The UI only talks to this origin and loads no third-party assets.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; connect-src 'self'; img-src 'self' data:; "+
				"style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	})
}
