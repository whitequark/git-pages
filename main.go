// Go insists on having this file here for `go install` to work properly.

package main

import (
	"runtime/debug"

	git_pages "codeberg.org/git-pages/git-pages/src"
)

// By default the version information is retrieved from VCS. If not available during build,
// override this variable using linker flags to change the displayed version.
// Example: `-ldflags "-X main.versionOverride=v1.2.3"`
var versionOverride = ""

func extractVersion() string {
	if versionOverride != "" {
		return versionOverride
	} else if buildInfo, ok := debug.ReadBuildInfo(); ok {
		return buildInfo.Main.Version
	} else {
		panic("version information not available")
	}
}

func main() {
	git_pages.Main(extractVersion())
}
