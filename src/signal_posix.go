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

func OnInterrupt(handler func()) {
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT)
	go func() {
		<-sigint
		signal.Stop(sigint)
		handler()
	}()
}
