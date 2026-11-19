// Package web embeds the demo dashboard.
package web

import "embed"

//go:embed index.html
var FS embed.FS
