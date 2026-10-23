//go:build !windows

package desktop

import (
	"errors"
	"syscall"
)

func addrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
