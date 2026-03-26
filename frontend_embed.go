package webscrcpy

import (
	"embed"
	"io/fs"
)

//go:embed all:frontend
var frontendFS embed.FS

// FrontendFS returns the embedded frontend filesystem rooted at frontend/.
func FrontendFS() (fs.FS, error) {
	return fs.Sub(frontendFS, "frontend")
}
