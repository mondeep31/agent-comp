// Package storage prepares VM disk files on the host.
//
// CloneFile is the per-sandbox hot path: it reflinks where the filesystem
// supports it (XFS, btrfs) and falls back to a sparse copy on ext4.
package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// CreateExt4 creates a sparse file of sizeMiB and formats it as ext4. Inode
// tables are initialised up front so guests never run lazy init after a restore.
func CreateExt4(ctx context.Context, path string, sizeMiB int) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := f.Truncate(int64(sizeMiB) << 20); err != nil {
		f.Close()
		return fmt.Errorf("sizing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	//nolint:gosec // fixed binary; path is created by us
	cmd := exec.CommandContext(ctx, "mkfs.ext4",
		"-q", "-F", "-m", "0",
		"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard",
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w: %s", path, err, out)
	}
	return nil
}

// LinkOrCopy hard links src to dst, copying when they are on different filesystems.
func LinkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return CloneFile(src, dst)
}
