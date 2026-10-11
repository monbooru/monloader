package web

import "embed"

// static/htmx.min.js is https://unpkg.com/htmx.org@2.0.11/dist/htmx.min.js,
// sha256 d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717.

//go:embed static templates
var FS embed.FS
