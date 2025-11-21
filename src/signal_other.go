//go:build !unix

package git_pages

func OnReload(handler func()) {
	// not implemented
}

func OnInterrupt(handler func()) {
	// not implemented
}
