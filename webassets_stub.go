//go:build !embedweb

// Package culturenodes carries the optional embedded web UI.
//
// The default build carries no web assets: `go build ./...` must work in a
// checkout that never ran npm (web/dist is gitignored). Production images
// build with `-tags embedweb` after the Dockerfile's node stage has
// produced web/dist, and the API server then serves the SPA (prd-spec
// §19.1: the UI is embedded in the Go service; a separate UI container is
// unnecessary).
package culturenodes

import "io/fs"

// WebAssets is the embedded SPA build, or nil when this binary was built
// without the embedweb tag.
func WebAssets() (fs.FS, bool) { return nil, false }
