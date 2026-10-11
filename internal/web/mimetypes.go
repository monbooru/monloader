package web

import "mime"

// Windows registry MIME types override Go's built-ins, and some machines map
// .css to text/plain. AddExtensionType loads that table first, so these win.
func init() {
	for ext, typ := range map[string]string{
		".css": "text/css; charset=utf-8",
		".js":  "text/javascript; charset=utf-8",
		".png": "image/png",
	} {
		if err := mime.AddExtensionType(ext, typ); err != nil {
			panic(err)
		}
	}
}
