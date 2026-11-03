package golden

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShapeAndKeyStrings(t *testing.T) {
	k := Key{Image: "base", Shape: Shape{VCPU: 2, MemoryMiB: 512, DiskMiB: 4096}}
	if got := k.String(); got != "base/2c-512m-4096d" {
		t.Fatalf("Key.String() = %q", got)
	}
}

func TestFingerprintTracksInputs(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	rootfs := filepath.Join(dir, "rootfs.ext4")
	for _, p := range []string{kernel, rootfs} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := New(Options{Dir: filepath.Join(dir, "golden")})
	if err != nil {
		t.Fatal(err)
	}
	img := Image{Name: "base", Kernel: kernel, Rootfs: rootfs, BootArgs: DefaultBootArgs}
	key := Key{Image: "base", Shape: Shape{VCPU: 1, MemoryMiB: 256, DiskMiB: 64}}

	fp := func(k Key, i Image) string {
		t.Helper()
		s, err := m.fingerprint(k, i)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	base := fp(key, img)
	if fp(key, img) != base {
		t.Fatal("fingerprint is not stable")
	}
	bigger := key
	bigger.Shape.MemoryMiB = 512
	if fp(bigger, img) == base {
		t.Error("shape change not detected")
	}
	if err := os.Chtimes(rootfs, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if fp(key, img) == base {
		t.Error("rootfs change not detected")
	}
}

func TestGetUnknownImage(t *testing.T) {
	m, err := New(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(t.Context(), Key{Image: "nope"}); !errors.Is(err, ErrUnknownImage) {
		t.Fatalf("got %v", err)
	}
}
