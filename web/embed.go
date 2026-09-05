// Package web embeds the templates, static assets and agent-facing documents.
package web

import (
	"embed"
	"io/fs"
)

// FS holds templates/, static/ and docs/.
//
//go:embed templates static docs
var FS embed.FS

// Static returns the static/ subtree rooted at its top (served under /static/).
func Static() fs.FS {
	sub, err := fs.Sub(FS, "static")
	if err != nil {
		// The directory is embedded at compile time; a failure here is a build bug.
		panic("web: static subtree missing: " + err.Error())
	}
	return sub
}
