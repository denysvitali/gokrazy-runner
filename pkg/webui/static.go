package webui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var staticFS embed.FS

// AssetsFS returns the assets sub-FS rooted so paths look like "index.html".
func AssetsFS() fs.FS {
	sub, err := fs.Sub(staticFS, "assets")
	if err != nil {
		// Should never happen: the assets directory is embedded at build time.
		panic(err)
	}
	return sub
}

// StaticHandler serves files from /assets/ at the given URL prefix (typically
// "/static/"). The prefix is stripped before lookup. Returns 404 for missing
// files via http.FileServer's default behavior.
func StaticHandler(prefix string) http.Handler {
	fileServer := http.FileServer(http.FS(AssetsFS()))
	return http.StripPrefix(prefix, fileServer)
}

// IndexHTML returns the contents of assets/index.html.
func IndexHTML() ([]byte, error) {
	return fs.ReadFile(staticFS, "assets/index.html")
}
