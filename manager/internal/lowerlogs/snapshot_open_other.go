//go:build !windows

package lowerlogs

import "os"

func openSnapshotFile(path string) (*os.File, error) { return os.Open(path) }
