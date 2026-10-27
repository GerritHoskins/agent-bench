//go:build windows

package bench

import (
	"fmt"
	"syscall"
)

// The OS releases this exclusive handle on process exit, including a crash.
func lockBundle(path string) (func(), error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, fmt.Errorf("bundle is locked or inaccessible: %w", err)
	}
	return func() { syscall.CloseHandle(h) }, nil
}
