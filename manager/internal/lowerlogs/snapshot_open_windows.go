//go:build windows

package lowerlogs

import (
	"golang.org/x/sys/windows"
	"os"
)

// Permit rotation while a query pins the old segment. Ordinary os.Open on
// Windows does not request FILE_SHARE_DELETE and would block rename/removal.
func openSnapshotFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open snapshot", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
