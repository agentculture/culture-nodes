//go:build embedweb

package culturenodes

import (
	"embed"
	"io/fs"
)

// The all: prefix keeps files beginning with _ or . (Vite emits neither
// today, but asset hashing conventions change).
//
//go:embed all:web/dist
var webDist embed.FS

// WebAssets is the embedded SPA build rooted at the dist directory.
func WebAssets() (fs.FS, bool) {
	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
