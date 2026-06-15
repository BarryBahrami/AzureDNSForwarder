// Package assets embeds the web/ directory so the server can serve it
// without runtime filesystem dependencies.
package assets

import "embed"

//go:embed all:web
var FS embed.FS
