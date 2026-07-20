package api

import (
	"embed"
	"io/fs"
)

//go:embed web/index.html web/qrcode.js
var webFiles embed.FS

func webFS() fs.FS {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
