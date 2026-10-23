// SPDX-License-Identifier: MIT
package rcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type Packages struct {
	Repository *Repository
	Root       string
	publish    sync.Mutex
}

func NewPackages(repository *Repository, root string) (*Packages, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("RCD package directory must be absolute and clean")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil || resolved != root {
		return nil, fmt.Errorf("RCD package directory must not contain symlinks")
	}
	if err := privateDirectory(filepath.Join(root, "incoming")); err != nil {
		return nil, err
	}
	return &Packages{Repository: repository, Root: root}, nil
}

func privateDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("RCD package directory must be private and not a symlink")
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *Packages) Upload(ctx context.Context, project int, actor string, input io.Reader) (Package, bool, error) {
	if project <= 0 || actor == "" {
		return Package{}, false, ErrInvalid
	}
	v, err := ValidateArchive(ctx, input, filepath.Join(s.Root, "incoming"))
	if err != nil {
		return Package{}, false, err
	}
	defer os.RemoveAll(v.Directory)
	if err = s.Repository.RequireRegisteredComponent(ctx, project, v.Manifest.Component); err != nil {
		return Package{}, false, err
	}
	for component := range v.Manifest.Compatibility.ComponentMinVersions {
		if err = s.Repository.RequireRegisteredComponent(ctx, project, component); err != nil {
			return Package{}, false, err
		}
	}
	// Only immutable publication/registration is serialized, never receiving or
	// validating the client stream. No native task is created by this operation.
	s.publish.Lock()
	defer s.publish.Unlock()
	existing, err := s.Repository.FindRelease(ctx, project, v.Manifest)
	if err == nil {
		if existing.ID != v.ID {
			return Package{}, false, ErrConflict
		}
		file, err := s.OpenArchive(project, existing)
		if err != nil {
			return Package{}, false, err
		}
		file.Close()
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Package{}, false, err
	}
	if err = s.Repository.CheckBinaryIdentity(ctx, project, v.Manifest); err != nil {
		return Package{}, false, err
	}
	parent := filepath.Join(s.Root, strconv.Itoa(project))
	if err = privateDirectory(parent); err != nil {
		return Package{}, false, err
	}
	destination := filepath.Join(parent, v.ID)
	if _, err = os.Lstat(destination); os.IsNotExist(err) {
		if err = syncDir(v.Directory); err == nil {
			err = os.Rename(v.Directory, destination)
		}
		if err == nil {
			err = syncDir(parent)
		}
		if err == nil {
			err = syncDir(filepath.Join(s.Root, "incoming"))
		}
	} else if err == nil {
		// A crash after file publication but before database commit leaves an
		// orphan. Revalidate the whole archive rather than trusting its filename.
		if err = privateDirectory(destination); err != nil {
			return Package{}, false, err
		}
		orphan, openErr := regularFile(filepath.Join(destination, "package.rcd.tar.gz"))
		if openErr != nil {
			return Package{}, false, openErr
		}
		checked, checkErr := ValidateArchive(ctx, orphan, filepath.Join(s.Root, "incoming"))
		orphan.Close()
		if checkErr != nil {
			return Package{}, false, checkErr
		}
		defer os.RemoveAll(checked.Directory)
		if checked.ID != v.ID {
			return Package{}, false, ErrConflict
		}
		// Also verify extracted files, used later by the native release task.
		for _, name := range []string{"binary", "manifest.json"} {
			good, err := fileDigest(filepath.Join(checked.Directory, name))
			if err != nil {
				return Package{}, false, err
			}
			actual, err := fileDigest(filepath.Join(destination, name))
			if err != nil || actual != good {
				return Package{}, false, fmt.Errorf("orphan package content is damaged")
			}
		}
		v.ArchiveSHA256, v.ArchiveSize = checked.ArchiveSHA256, checked.ArchiveSize
	}
	if err != nil {
		return Package{}, false, err
	}
	p, err := s.Repository.Insert(ctx, project, v, actor)
	return p, err == nil, err
}

func regularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("RCD package file is not regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		f.Close()
		return nil, errors.New("RCD package file changed while opening")
	}
	return f, nil
}

func fileDigest(path string) (string, error) {
	f, err := regularFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Packages) OpenArchive(project int, p Package) (*os.File, error) {
	if project <= 0 || !digestPattern.MatchString(p.ID) {
		return nil, ErrInvalid
	}
	directory := filepath.Join(s.Root, strconv.Itoa(project), p.ID)
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil || resolved != directory {
		return nil, errors.New("package storage is missing or redirected")
	}
	f, err := regularFile(filepath.Join(directory, "package.rcd.tar.gz"))
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxArchiveBytes+1))
	if err != nil || n != p.ArchiveSize || hex.EncodeToString(h.Sum(nil)) != p.ArchiveSHA256 {
		f.Close()
		return nil, errors.New("stored package checksum mismatch")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
