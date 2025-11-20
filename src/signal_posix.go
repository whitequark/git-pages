//go:build unix

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
