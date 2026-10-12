//go:build unix

package integrations

import (
	"os"
	"syscall"
)

func openNativeArchiveInput(name string) (*os.File, error) {
	return os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
}
