package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneFilePreservesContentAroundHoles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 8 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	head := []byte("head-block")
	tail := []byte("tail-block")
	if _, err := f.WriteAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(tail, size-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(dir, "dst")
	if err := CloneFile(src, dst); err != nil {
		t.Fatalf("CloneFile: %v", err)
	}
	want, _ := os.ReadFile(src)
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("clone content differs from source")
	}
}

func TestCloneFileRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	for _, p := range []string{src, dst} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := CloneFile(src, dst); err == nil {
		t.Fatal("expected an error when dst exists")
	}
}
