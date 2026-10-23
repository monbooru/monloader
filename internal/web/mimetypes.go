package web

import "mime"

// Windows registry MIME types override Go's built-ins. AddExtensionType loads
// that table first, so this wins; webui pins .css and .js.
func init() {
	if err := mime.AddExtensionType(".png", "image/png"); err != nil {
		panic(err)
	}
}
