package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/afero"
)

func TestBindingFileSystem(t *testing.T) {
	direct := t.TempDir()
	cow := t.TempDir()
	if err := os.WriteFile(filepath.Join(cow, "base.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}

	factory, err := bindingFileSystem(
		[]string{direct + "=/workspace"},
		[]string{cow + "=/packages"},
	)
	if err != nil {
		t.Fatal(err)
	}
	filesystem, err := factory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = filesystem.Cleanup() })
	if len(filesystem.Mounts) != 3 {
		t.Fatalf("mount count = %d, want root plus two binds", len(filesystem.Mounts))
	}

	byGuest := make(map[string]afero.Fs)
	for _, mount := range filesystem.Mounts {
		byGuest[mount.GuestPath] = mount.FS
	}
	if err := afero.WriteFile(byGuest["/workspace"], "direct.txt", []byte("direct"), 0o644); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(direct, "direct.txt")); err != nil || string(contents) != "direct" {
		t.Fatalf("direct bind = %q, %v", contents, err)
	}
	if err := afero.WriteFile(byGuest["/packages"], "base.txt", []byte("overlay"), 0o644); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(cow, "base.txt")); err != nil || string(contents) != "base" {
		t.Fatalf("COW base = %q, %v", contents, err)
	}
}

func TestBindingFileSystemRejectsDuplicateGuestPath(t *testing.T) {
	directory := t.TempDir()
	if _, err := bindingFileSystem(
		[]string{directory + "=/data"},
		[]string{directory + "=/data"},
	); err == nil {
		t.Fatal("expected duplicate guest path error")
	}
}
