package assets

import "embed"

//go:embed *.tmpl *.svg
var Templates embed.FS
