package mount

import (
	"fmt"
	"os"
	"syscall"

	"github.com/pkg/errors"
)

// Proc mounts proc filesystem.
func Proc(target string) error {
	if err := os.Mkdir(target, 0o755); err != nil && !os.IsExist(err) {
		return errors.WithStack(err)
	}
	if err := syscall.Mount("none", target, "proc", 0, ""); err != nil {
		return errors.WithStack(fmt.Errorf("mounting proc failed: %w", err))
	}
	return nil
}
