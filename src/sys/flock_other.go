//go:build !unix

package sys

import (
	"fmt"
	"os"
)

func FileLock(file *os.File) error {
	return fmt.Errorf("unimplemented")
}

func FileUnlock(file *os.File) error {
	return fmt.Errorf("unimplemented")
}
