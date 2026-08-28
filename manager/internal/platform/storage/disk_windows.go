//go:build windows

package storage

import (
	"fmt"
	"math"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func DiskFree(path string) (int64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	pointer, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &free, nil, nil); err != nil {
		return 0, fmt.Errorf("read disk free space: %w", err)
	}
	if free > uint64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(free), nil
}
