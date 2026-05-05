//go:build embed

package web

import _ "embed"

//go:embed dist/viewer.js
var ViewerJS []byte

//go:embed index.html
var ViewerHTML []byte
