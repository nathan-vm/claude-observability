package service

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWritePrivateFile_NewFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "svc")
	if err := writePrivateFile(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestWritePrivateFile_TightensExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "svc")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // defeat umask
		t.Fatal(err)
	}
	if err := writePrivateFile(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600 after reinstall", got)
	}
	if data, _ := os.ReadFile(path); string(data) != "new" {
		t.Errorf("content = %q, want rewritten", data)
	}
}
