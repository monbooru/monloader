package web

import "embed"

//go:embed static templates plugins/themes/light/theme.css plugins/themes/legacy-dark plugins/themes/legacy-light
var FS embed.FS
