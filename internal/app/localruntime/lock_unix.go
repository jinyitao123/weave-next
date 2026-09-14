//go:build !windows

package localruntime

import (
	"os"
	"path/filepath"
	"syscall"
)

func lockHostRoot(root string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(root, ".host.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
