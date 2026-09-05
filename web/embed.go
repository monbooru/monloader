package web

import "embed"

// htmx is vendored rather than fetched at runtime: the UI has to work on a LAN
// with no egress, and a CDN is one more thing that can change under a running
// install. The bundle is htmx 2.0.10 from
// https://unpkg.com/htmx.org@2.0.10/dist/htmx.min.js,
// sha256 71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de,
// so a later reader can check it against upstream the way the other bundled
// artifacts are checked. monbooru is still on the 1.x line, which differs in
// swap defaults and attribute inheritance, so an htmx idiom does not carry
// between the two trees the way the rest of the shared shape does.

//go:embed static templates
var FS embed.FS
