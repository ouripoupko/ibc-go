// Package ibcjs holds the built JavaScript client (the npm package
// @ouripoupko/ibc, built from src/ into dist/), so that the node can serve it
// to its own web pages.
package ibcjs

import "embed"

// Dist is the built client: dist/index.js and the files it imports.
//
//go:embed all:dist
var Dist embed.FS
