// SPDX-License-Identifier: MIT
package rcd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type ValidatedArchive struct {
	Manifest      Manifest
	ID            string
	ArchiveSHA256 string
	ArchiveSize   int64
	Directory     string
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// ValidateArchive writes only server-selected names in a newly-created private
// directory. It never executes an uploaded binary or trusts a tar path.
func ValidateArchive(ctx context.Context, input io.Reader, incoming string) (result ValidatedArchive, err error) {
	dir, err := os.MkdirTemp(incoming, "upload-")
	if err != nil {
		return result, err
	}
	result.Directory = dir
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	archive, err := os.OpenFile(filepath.Join(dir, "package.rcd.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer archive.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(contextReader{ctx, input}, MaxArchiveBytes+1))
	if err != nil {
		return result, err
	}
	if n > MaxArchiveBytes {
		return result, ErrTooLarge
	}
	result.ArchiveSize, result.ArchiveSHA256 = n, hex.EncodeToString(hash.Sum(nil))
	if err = archive.Sync(); err != nil {
		return result, err
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	buffered := bufio.NewReader(contextReader{ctx, archive})
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return result, fmt.Errorf("%w: gzip header", ErrInvalid)
	}
	defer gz.Close()
	gz.Multistream(false)
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var manifestRaw []byte
	var binarySize int64
	var binaryHash string
	for {
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return result, fmt.Errorf("%w: tar header: %v", ErrInvalid, nextErr)
		}
		if header.Format != tar.FormatUSTAR || header.Typeflag != tar.TypeReg || header.Linkname != "" || seen[header.Name] {
			return result, fmt.Errorf("%w: only distinct regular USTAR files allowed", ErrInvalid)
		}
		seen[header.Name] = true
		switch header.Name {
		case "manifest.json":
			if header.Size <= 0 || header.Size > MaxManifestBytes {
				return result, ErrTooLarge
			}
			manifestRaw, err = io.ReadAll(tr)
		case "binary":
			if header.Size < 64 || header.Size > MaxBinaryBytes {
				return result, ErrTooLarge
			}
			binary, openErr := os.OpenFile(filepath.Join(dir, "binary"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if openErr != nil {
				return result, openErr
			}
			binaryDigest := sha256.New()
			binarySize, err = io.Copy(io.MultiWriter(binary, binaryDigest), tr)
			if err == nil {
				err = binary.Sync()
			}
			closeErr := binary.Close()
			if err == nil {
				err = closeErr
			}
			binaryHash = hex.EncodeToString(binaryDigest.Sum(nil))
		default:
			return result, fmt.Errorf("%w: unexpected archive member", ErrInvalid)
		}
		if err != nil {
			return result, fmt.Errorf("%w: truncated member: %v", ErrInvalid, err)
		}
	}
	if len(seen) != 2 {
		return result, fmt.Errorf("%w: manifest and binary required", ErrInvalid)
	}
	// tar stops at its terminator; finish gzip to validate its CRC and reject
	// non-zero trailers, concatenated archives and excessive tar padding.
	padding, err := io.ReadAll(io.LimitReader(gz, 10241))
	if err != nil || len(padding) > 10240 || !bytes.Equal(padding, make([]byte, len(padding))) {
		return result, fmt.Errorf("%w: archive trailer", ErrInvalid)
	}
	if _, err = buffered.ReadByte(); err != io.EOF {
		return result, fmt.Errorf("%w: trailing gzip data", ErrInvalid)
	}
	if err = StrictJSON(manifestRaw, &result.Manifest); err != nil {
		return result, err
	}
	if err = result.Manifest.Validate(); err != nil {
		return result, err
	}
	if binarySize != result.Manifest.BinarySize || binaryHash != result.Manifest.BinarySHA256 {
		return result, fmt.Errorf("%w: binary size or SHA-256 mismatch", ErrInvalid)
	}
	binary, err := elf.Open(filepath.Join(dir, "binary"))
	if err != nil {
		return result, fmt.Errorf("%w: ELF: %v", ErrInvalid, err)
	}
	defer binary.Close()
	want := elf.EM_X86_64
	if Architecture(result.Manifest.Target) == "arm64" {
		want = elf.EM_AARCH64
	}
	if binary.Class != elf.ELFCLASS64 || binary.Data != elf.ELFDATA2LSB || binary.Machine != want || (binary.Type != elf.ET_EXEC && binary.Type != elf.ET_DYN) {
		return result, fmt.Errorf("%w: ELF target mismatch", ErrInvalid)
	}
	canonical, id := result.Manifest.Canonical()
	result.ID = id
	file, err := os.OpenFile(filepath.Join(dir, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return result, err
	}
	_, err = file.Write(canonical)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	return result, err
}
