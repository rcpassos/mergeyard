// Package webassets contains the dashboard assets built before Go compilation.
package webassets

import "embed"

// Files embeds templates and generated, self-hosted frontend assets.
//
//go:embed templates/*.html static
var Files embed.FS
