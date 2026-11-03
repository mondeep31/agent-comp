// Package testenv holds helpers for integration tests that boot real VMs.
package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mondeep31/agent-comp/internal/storage"
)

// CI returns the directory holding the Firecracker CI kernel and rootfs,
// skipping the test when not root or when artifacts are missing.
func CI(t testing.TB) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("integration tests need root (jailer)")
	}
	dir := os.Getenv("AC_CI_DIR")
	if dir == "" {
		dir = "/var/lib/agent-comp/ci"
	}
	if _, err := os.Stat(filepath.Join(dir, "vmlinux")); err != nil {
		t.Skipf("CI artifacts missing (run scripts/host/fetch-ci-artifacts.sh): %v", err)
	}
	return dir
}

// RepoRoot returns the repository root, based on this file's location.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Run executes a command and fails the test on error.
func Run(t testing.TB, name string, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}

// File is content to install into a test rootfs.
type File struct {
	Data []byte
	Mode os.FileMode
}

// Rootfs copies the CI rootfs to dst, installs files (keyed by absolute path)
// and makes the result read-only, as the driver expects of a base image.
func Rootfs(t testing.TB, ci, dst string, files map[string]File) {
	t.Helper()
	if err := storage.CloneFile(filepath.Join(ci, "rootfs.ext4"), dst); err != nil {
		t.Fatal(err)
	}
	mnt := t.TempDir()
	Run(t, "mount", "-o", "loop", dst, mnt)
	for path, f := range files {
		p := filepath.Join(mnt, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, f.Mode); err != nil {
			t.Fatal(err)
		}
	}
	Run(t, "umount", mnt)
	if err := os.Chmod(dst, 0o444); err != nil {
		t.Fatal(err)
	}
}
