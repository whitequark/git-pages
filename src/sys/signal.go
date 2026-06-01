// See https://pkg.go.dev/os/signal#hdr-Windows for a description of what this module
// will do on Windows.

package sys

import (
	"os"
	"os/signal"
	"syscall"
)

func WaitForInterrupt() {
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT, syscall.SIGTERM)
	<-sigint
	signal.Stop(sigint)
}
