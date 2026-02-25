package dashboard

import (
	_ "embed"
	"os"
)

//go:embed static/index.html
var indexHTML []byte

// devStaticDir, when non-empty, causes the dashboard to serve index.html
// from the filesystem instead of the embedded copy. Set via
// DASHBOARD_STATIC_DIR env var for hot-reload during development.
var devStaticDir = os.Getenv("DASHBOARD_STATIC_DIR")

// getIndexHTML returns the embedded HTML or reads from disk in dev mode.
func getIndexHTML() []byte {
	if devStaticDir == "" {
		return indexHTML
	}
	data, err := os.ReadFile(devStaticDir + "/index.html")
	if err != nil {
		return indexHTML // fall back to embedded
	}
	return data
}
