//go:build !windows

package integrations

import (
	"os"
	"syscall"
)

func nativeGuardOpenFlags() int { return os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_NOFOLLOW }
