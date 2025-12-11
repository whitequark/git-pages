// See https://pkg.go.dev/os/signal#hdr-Windows for a description of what this module
// will do on Windows (tl;dr nothing calls the reload handler, the interrupt handler works
// more or less how you'd expect).

package git_pages

import (
	"os"
	"os/signal"
	"syscall"
)

func OnReload(handler func()) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for {
			<-sighup
			handler()
		}
	}()
}

func WaitForInterrupt() {
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)
	<-sigint
	signal.Stop(sigint)
}
