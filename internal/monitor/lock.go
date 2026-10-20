package monitor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireLock holds one kernel lock per data directory. Never unlink the file:
// doing so would let a second process lock a different inode at the same path.
func acquireLock(database string) (*os.File, error) {
	absolute, err := filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	directory := filepath.Dir(absolute)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, fmt.Errorf("create monitor data directory: %w", err)
	}
	file, err := os.OpenFile(filepath.Join(directory, ".aegis-monitor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open monitor lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another monitor is already using this data directory")
		}
		return nil, fmt.Errorf("acquire monitor lock: %w", err)
	}
	return file, nil
}
