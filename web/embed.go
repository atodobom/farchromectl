// Package web embute os arquivos da UI no binário via //go:embed.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html app.js style.css manifest.webmanifest icon.svg
var files embed.FS

// Files devolve o FS embutido (raiz = index.html, app.js, ...).
func Files() fs.FS { return files }
