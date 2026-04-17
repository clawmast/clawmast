// Package embed bundles the static worker UI into the binary via
// go:embed.
//
// Iteration 0 ships a hand-written single-page HTML + CSS + JS
// dashboard (architecture/refactor.md §9: "UI shows version, check
// for updates button") directly checked into `internal/embed/dist/`.
// Later iterations may replace that directory with the output of a
// React + Vite build copied in by the release pipeline; consumers of
// Assets do not need to change because the served paths stay the same.
//
// The `all:` prefix keeps dotfiles (such as `.gitkeep` or `.well-known`
// manifests) if they ever appear; at the moment there are none.
package embed

import embedfs "embed"

// Assets is the embedded filesystem rooted at internal/embed/dist.
// Callers typically do:
//
//	uiFS, _ := fs.Sub(embed.Assets, "dist")
//	mux.Handle("/", http.FileServerFS(uiFS))
//
//go:embed all:dist
var Assets embedfs.FS
