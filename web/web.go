// Package web is the node's web interface: static pages, built into the
// binary, that talk to the node's API from the browser and sign with passkeys.
// The pages use the JavaScript client (package ibcjs), served at /js/.
package web

import (
	"embed"
	"io/fs"
	"net/http"

	ibcjs "github.com/ouripoupko/ibc-go/js"
)

//go:embed all:static
var files embed.FS

// Handler serves the web interface's files.
func Handler() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	client, err := fs.Sub(ibcjs.Dist, "dist")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServerFS(static))
	mux.Handle("/js/", http.StripPrefix("/js/", http.FileServerFS(client)))
	server := mux
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		server.ServeHTTP(w, r)
	})
}
