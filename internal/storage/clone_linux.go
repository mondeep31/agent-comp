package storage

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// CloneFile copies src to a new file dst, preserving holes. It tries a reflink
// first, then copies only the data extents found with SEEK_DATA/SEEK_HOLE.
func CloneFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(dst)
		}
	}()

	if unix.IoctlFileClone(int(out.Fd()), int(in.Fd())) == nil {
		return nil
	}
	if err := out.Truncate(st.Size()); err != nil {
		return err
	}
	return copyExtents(in, out, st.Size())
}

func copyExtents(in, out *os.File, size int64) error {
	inFD := int(in.Fd())
	var off int64
	for off < size {
		data, err := unix.Seek(inFD, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			return nil // only holes remain
		}
		if err != nil {
			return fmt.Errorf("seek data: %w", err)
		}
		hole, err := unix.Seek(inFD, data, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("seek hole: %w", err)
		}
		if err := copyRange(in, out, data, hole-data); err != nil {
			return err
		}
		off = hole
	}
	return nil
}

func copyRange(in, out *os.File, off, n int64) error {
	for n > 0 {
		written, err := unix.CopyFileRange(int(in.Fd()), &off, int(out.Fd()), ptr(off), int(n), 0)
		if err != nil {
			if errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
				return copyRangeUserspace(in, out, off, n)
			}
			return fmt.Errorf("copy_file_range: %w", err)
		}
		if written == 0 {
			return io.ErrUnexpectedEOF
		}
		n -= int64(written)
	}
	return nil
}

// ptr returns a fresh pointer so CopyFileRange advances the output offset in lockstep.
func ptr(v int64) *int64 { return &v }

func copyRangeUserspace(in, out *os.File, off, n int64) error {
	_, err := io.Copy(io.NewOffsetWriter(out, off), io.NewSectionReader(in, off, n))
	return err
}
