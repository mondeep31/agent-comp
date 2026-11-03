//go:build !linux

package storage

import (
	"io"
	"os"
)

// CloneFile copies src to a new file dst. Non-Linux builds exist only for
// development and do not preserve holes.
func CloneFile(src, dst string) error {
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
	if err := out.Chmod(st.Mode().Perm()); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
