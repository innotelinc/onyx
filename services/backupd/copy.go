package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// copyLocalBackup copies a source path into a private run directory below the
// configured target. It refuses relative paths and symlinks so a local job
// cannot escape its declared source tree.
func copyLocalBackup(source, target, runID string) (int64, error) {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) {
		return 0, fmt.Errorf("local backup source and target must be absolute paths")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return 0, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return 0, err
	}
	if source == target || strings.HasPrefix(target, source+string(os.PathSeparator)) {
		return 0, fmt.Errorf("backup target must not be inside source")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return 0, fmt.Errorf("stat source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return 0, fmt.Errorf("source symlinks are not supported")
	}

	destination := filepath.Join(target, runID)
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return 0, fmt.Errorf("create target: %w", err)
	}
	if info.IsDir() {
		bytes, err := copyDirectory(source, destination)
		if err != nil {
			_ = os.RemoveAll(destination)
		}
		return bytes, err
	}
	bytes, err := copyOne(source, filepath.Join(destination, filepath.Base(source)))
	if err != nil {
		_ = os.RemoveAll(destination)
		return bytes, err
	}
	if err := syncDir(destination); err != nil {
		_ = os.RemoveAll(destination)
		return bytes, err
	}
	return bytes, nil
}

func copyDirectory(source, destination string) (int64, error) {
	var total int64
	err := filepath.Walk(source, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not supported: %s", path)
		}
		dst := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported source entry: %s", path)
		}
		bytes, err := copyOne(path, dst)
		total += bytes
		return err
	})
	if err != nil {
		return total, err
	}
	return total, syncDir(destination)
}

func copyOne(source, destination string) (int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return 0, err
	}
	tmp := destination + ".onyx-partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return 0, err
	}
	bytes, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return bytes, copyErr
	}
	if syncErr != nil {
		_ = os.Remove(tmp)
		return bytes, syncErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return bytes, closeErr
	}
	if err := os.Rename(tmp, destination); err != nil {
		_ = os.Remove(tmp)
		return bytes, err
	}
	return bytes, nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
