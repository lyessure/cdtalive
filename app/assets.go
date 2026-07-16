package webassets

import "embed"

// Pages is the single shared frontend used by both the Python and Go builds.
//
//go:embed init.html dashboard.html
var Pages embed.FS
