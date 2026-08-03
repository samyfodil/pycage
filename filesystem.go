package pycage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/samyfodil/wazy"
	"github.com/spf13/afero"
)

// FSMount exposes an Afero filesystem at GuestPath. Writable mounts use
// Afero's full mutation API; read-only mounts are passed through io/fs.FS.
type FSMount struct {
	GuestPath string
	FS        afero.Fs
	ReadOnly  bool
}

// FileSystem describes the mounts created for one sandbox. Cleanup, when set,
// runs after the component closes.
type FileSystem struct {
	Mounts  []FSMount
	Cleanup func() error
}

// FileSystemFactory creates an independent filesystem view for each sandbox.
type FileSystemFactory func() (FileSystem, error)

// Mount exposes an arbitrary Afero filesystem at a guest path.
func Mount(guestPath string, filesystem afero.Fs) FSMount {
	return FSMount{GuestPath: guestPath, FS: filesystem}
}

// ReadOnlyMount exposes an arbitrary Afero filesystem without guest writes.
func ReadOnlyMount(guestPath string, filesystem afero.Fs) FSMount {
	return FSMount{GuestPath: guestPath, FS: filesystem, ReadOnly: true}
}

// Bind exposes a host directory through Afero's BasePathFs. Guest writes
// modify the host directory. Use CopyOnWrite with this mount's FS to isolate
// changes from the host.
func Bind(guestPath, hostDirectory string) FSMount {
	return Mount(guestPath, afero.NewBasePathFs(afero.NewOsFs(), hostDirectory))
}

// CopyOnWrite creates an Afero COW mount. Reads fall through to base and file
// writes copy into layer. Afero rejects rename/remove of base-only entries.
func CopyOnWrite(guestPath string, base, layer afero.Fs) FSMount {
	return Mount(guestPath, afero.NewCopyOnWriteFs(base, layer))
}

// StaticFileSystem uses the supplied mount instances for every sandbox made
// by an Engine. State is therefore shared; use a custom FileSystemFactory when
// each sandbox needs fresh layers.
func StaticFileSystem(mounts ...FSMount) FileSystemFactory {
	return func() (FileSystem, error) {
		return FileSystem{Mounts: append([]FSMount(nil), mounts...)}, nil
	}
}

// DefaultFileSystem creates pycage's isolated default: a fresh Afero memory
// layer over a private temporary-directory base. The base is removed on close.
func DefaultFileSystem() (FileSystem, error) {
	directory, err := os.MkdirTemp("", "pycage-fs-")
	if err != nil {
		return FileSystem{}, fmt.Errorf("pycage: create temporary filesystem: %w", err)
	}
	base := afero.NewBasePathFs(afero.NewOsFs(), directory)
	layer := afero.NewMemMapFs()
	return FileSystem{
		Mounts: []FSMount{CopyOnWrite("/", base, layer)},
		Cleanup: func() error {
			return os.RemoveAll(directory)
		},
	}, nil
}

type mountedFS struct {
	guestPath string
	fs        afero.Fs
	readOnly  bool
}

type sandboxFilesystem struct {
	mounts  []mountedFS
	cleanup func() error
}

func newSandboxFilesystem(factory FileSystemFactory) (*sandboxFilesystem, wazy.FSConfig, error) {
	if factory == nil {
		factory = DefaultFileSystem
	}
	configured, err := factory()
	if err != nil {
		return nil, nil, err
	}
	cleanup := configured.Cleanup
	if cleanup == nil {
		cleanup = func() error { return nil }
	}
	fail := func(err error) (*sandboxFilesystem, wazy.FSConfig, error) {
		return nil, nil, errors.Join(err, cleanup())
	}
	if len(configured.Mounts) == 0 {
		return fail(fmt.Errorf("pycage: filesystem has no mounts"))
	}

	mounts := make([]mountedFS, 0, len(configured.Mounts))
	seen := make(map[string]bool, len(configured.Mounts))
	config := wazy.NewFSConfig()
	for _, mount := range configured.Mounts {
		guestPath, err := cleanMountPath(mount.GuestPath)
		if err != nil {
			return fail(err)
		}
		if mount.FS == nil {
			return fail(fmt.Errorf("pycage: filesystem mounted at %q is nil", guestPath))
		}
		if seen[guestPath] {
			return fail(fmt.Errorf("pycage: duplicate filesystem mount %q", guestPath))
		}
		seen[guestPath] = true
		mounts = append(mounts, mountedFS{guestPath: guestPath, fs: mount.FS, readOnly: mount.ReadOnly})
		if mount.ReadOnly {
			config = config.WithFSMount(afero.NewIOFS(mount.FS), guestPath)
		} else {
			config = config.WithSysFSMount(newAferoSysFS(mount.FS), guestPath)
		}
	}
	if !seen["/"] {
		return fail(fmt.Errorf("pycage: filesystem must mount guest root %q", "/"))
	}
	sort.SliceStable(mounts, func(left, right int) bool {
		return len(mounts[left].guestPath) > len(mounts[right].guestPath)
	})
	return &sandboxFilesystem{mounts: mounts, cleanup: cleanup}, config, nil
}

func cleanMountPath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("pycage: invalid filesystem mount %q", name)
	}
	if name == "" || name == "." || name == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("pycage: filesystem mount %q must be absolute", name)
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." {
			return "", fmt.Errorf("pycage: invalid filesystem mount %q", name)
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "/" || strings.HasPrefix(cleaned, "/../") {
		return "", fmt.Errorf("pycage: invalid filesystem mount %q", name)
	}
	return cleaned, nil
}

func (s *sandboxFilesystem) resolve(name string) (int, string, error) {
	if name == "" || name == "." || name == "/" {
		name = "/"
	} else {
		var err error
		name, err = cleanGuestPath(name)
		if err != nil {
			return -1, "", err
		}
	}
	for index, mount := range s.mounts {
		if mount.guestPath == "/" || name == mount.guestPath || strings.HasPrefix(name, mount.guestPath+"/") {
			relative := strings.TrimPrefix(name, mount.guestPath)
			return index, aferoName(strings.TrimPrefix(relative, "/")), nil
		}
	}
	return -1, "", fmt.Errorf("pycage: no filesystem mounted for %q", name)
}

func (s *sandboxFilesystem) writeFile(name string, data []byte) error {
	index, relative, err := s.resolve(name)
	if err != nil {
		return err
	}
	mount := s.mounts[index]
	if mount.readOnly {
		return fmt.Errorf("pycage: filesystem path %q is read-only", name)
	}
	if err := mount.fs.MkdirAll(filepath.Dir(relative), 0o755); err != nil {
		return err
	}
	return afero.WriteFile(mount.fs, relative, data, 0o644)
}

func (s *sandboxFilesystem) readFile(name string) ([]byte, error) {
	index, relative, err := s.resolve(name)
	if err != nil {
		return nil, err
	}
	return afero.ReadFile(s.mounts[index].fs, relative)
}

func (s *sandboxFilesystem) remove(name string) error {
	index, relative, err := s.resolve(name)
	if err != nil {
		return err
	}
	if s.mounts[index].readOnly {
		return fmt.Errorf("pycage: filesystem path %q is read-only", name)
	}
	return s.mounts[index].fs.Remove(relative)
}

func (s *sandboxFilesystem) removeAll(name string) error {
	index, relative, err := s.resolve(name)
	if err != nil {
		return err
	}
	if s.mounts[index].readOnly {
		return fmt.Errorf("pycage: filesystem path %q is read-only", name)
	}
	err = s.mounts[index].fs.RemoveAll(relative)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *sandboxFilesystem) listFiles(root string) (map[string][]byte, error) {
	if root == "" || root == "." || root == "/" {
		root = "/"
	} else {
		var err error
		root, err = cleanGuestPath(root)
		if err != nil {
			return nil, err
		}
	}
	files := make(map[string][]byte)
	for index, mount := range s.mounts {
		var walkRoot, guestBase string
		switch {
		case mount.guestPath == "/" || root == mount.guestPath || strings.HasPrefix(root, mount.guestPath+"/"):
			resolved, relative, resolveErr := s.resolve(root)
			if resolveErr != nil || resolved != index {
				continue
			}
			walkRoot, guestBase = relative, root
		case root == "/" || strings.HasPrefix(mount.guestPath, root+"/"):
			walkRoot, guestBase = ".", mount.guestPath
		default:
			continue
		}
		err := afero.Walk(mount.fs, walkRoot, func(name string, info fs.FileInfo, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) && name == walkRoot {
					return nil
				}
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			relative, relativeErr := filepath.Rel(walkRoot, name)
			if relativeErr != nil {
				return relativeErr
			}
			guestName := path.Join(guestBase, filepath.ToSlash(relative))
			if !strings.HasPrefix(guestName, "/") {
				guestName = "/" + guestName
			}
			owner, _, resolveErr := s.resolve(guestName)
			if resolveErr != nil || owner != index {
				return nil
			}
			contents, readErr := afero.ReadFile(mount.fs, name)
			if readErr != nil {
				return readErr
			}
			files[guestName] = contents
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (s *sandboxFilesystem) close() error {
	return s.cleanup()
}
