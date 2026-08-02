// Command crashcause is the crashcause CLI entrypoint.
package main

import (
	"os"

	"github.com/ryckakas/crashcause/internal/cli"
)

// version, commit, and date are injected at build time via linker flags,
// e.g.:
//
//	go build -ldflags "-X main.version=1.2.3 -X main.commit=abc1234 -X main.date=2026-08-02" ./cmd/crashcause
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	os.Exit(cli.Execute(cli.BuildInfo{
		Version: version,
		Commit:  commit,
		Date:    date,
	}))
}
