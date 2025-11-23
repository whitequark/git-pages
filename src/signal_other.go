//go:build !unix

package git_pages

func OnReload(handler func()) {
	// not implemented
}

func WaitForInterrupt() {
	for {
		// Ctrl+C not supported
	}
}
