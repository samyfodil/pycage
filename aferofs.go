package pycage

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/samyfodil/wazy/sys"
	"github.com/spf13/afero"
)

// aferoSysFS is the narrow protocol adapter between an Afero filesystem and
// Wazy's writable WASI filesystem interface. Afero owns all storage semantics.
type aferoSysFS struct {
	sys.UnimplementedFS
	fs afero.Fs
}

func newAferoSysFS(filesystem afero.Fs) sys.FS {
	return &aferoSysFS{fs: filesystem}
}

func (a *aferoSysFS) OpenFile(name string, flag sys.Oflag, perm fs.FileMode) (sys.File, sys.Errno) {
	openFlag := 0
	switch flag & 3 {
	case sys.O_RDWR:
		openFlag = os.O_RDWR
	case sys.O_WRONLY:
		openFlag = os.O_WRONLY
	default:
		openFlag = os.O_RDONLY
	}
	for _, pair := range []struct {
		wazy sys.Oflag
		host int
	}{
		{sys.O_APPEND, os.O_APPEND},
		{sys.O_CREAT, os.O_CREATE},
		{sys.O_EXCL, os.O_EXCL},
		{sys.O_SYNC, os.O_SYNC},
		{sys.O_TRUNC, os.O_TRUNC},
	} {
		if flag&pair.wazy != 0 {
			openFlag |= pair.host
		}
	}
	file, err := a.fs.OpenFile(aferoName(name), openFlag, perm)
	if err != nil {
		return nil, sys.UnwrapOSError(err)
	}
	if flag&sys.O_DIRECTORY != 0 {
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			return nil, sys.UnwrapOSError(statErr)
		}
		if !info.IsDir() {
			file.Close()
			return nil, sys.ENOTDIR
		}
	}
	return &aferoSysFile{file: file, filesystem: a.fs, name: aferoName(name), appendMode: flag&sys.O_APPEND != 0}, 0
}

func (a *aferoSysFS) Lstat(name string) (sys.Stat_t, sys.Errno) {
	return a.Stat(name)
}

func (a *aferoSysFS) Stat(name string) (sys.Stat_t, sys.Errno) {
	info, err := a.fs.Stat(aferoName(name))
	if err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err)
	}
	return sys.NewStat_t(info), 0
}

func (a *aferoSysFS) Mkdir(name string, perm fs.FileMode) sys.Errno {
	return sys.UnwrapOSError(a.fs.Mkdir(aferoName(name), perm))
}

func (a *aferoSysFS) Chmod(name string, perm fs.FileMode) sys.Errno {
	return sys.UnwrapOSError(a.fs.Chmod(aferoName(name), perm))
}

func (a *aferoSysFS) Rename(from, to string) sys.Errno {
	return sys.UnwrapOSError(a.fs.Rename(aferoName(from), aferoName(to)))
}

func (a *aferoSysFS) Rmdir(name string) sys.Errno {
	return sys.UnwrapOSError(a.fs.Remove(aferoName(name)))
}

func (a *aferoSysFS) Unlink(name string) sys.Errno {
	return sys.UnwrapOSError(a.fs.Remove(aferoName(name)))
}

func (a *aferoSysFS) Utimens(name string, accessNanos, modifyNanos int64) sys.Errno {
	return sys.UnwrapOSError(a.fs.Chtimes(
		aferoName(name),
		time.Unix(0, accessNanos),
		time.Unix(0, modifyNanos),
	))
}

type aferoSysFile struct {
	sys.UnimplementedFile
	file       afero.File
	filesystem afero.Fs
	name       string
	appendMode bool
}

func (f *aferoSysFile) stat() (sys.Stat_t, sys.Errno) {
	info, err := f.file.Stat()
	if err != nil {
		return sys.Stat_t{}, sys.UnwrapOSError(err)
	}
	return sys.NewStat_t(info), 0
}

func (f *aferoSysFile) Dev() (uint64, sys.Errno) {
	stat, errno := f.stat()
	return stat.Dev, errno
}

func (f *aferoSysFile) Ino() (sys.Inode, sys.Errno) {
	stat, errno := f.stat()
	return stat.Ino, errno
}

func (f *aferoSysFile) IsDir() (bool, sys.Errno) {
	info, err := f.file.Stat()
	if err != nil {
		return false, sys.UnwrapOSError(err)
	}
	return info.IsDir(), 0
}

func (f *aferoSysFile) IsAppend() bool { return f.appendMode }

func (f *aferoSysFile) Stat() (sys.Stat_t, sys.Errno) { return f.stat() }

func (f *aferoSysFile) Read(buffer []byte) (int, sys.Errno) {
	count, err := f.file.Read(buffer)
	return count, sys.UnwrapOSError(err)
}

func (f *aferoSysFile) Pread(buffer []byte, offset int64) (int, sys.Errno) {
	count, err := f.file.ReadAt(buffer, offset)
	return count, sys.UnwrapOSError(err)
}

// Seek implements Wazy's experimental sysfs File, not io.Seeker, so it returns
// a sys.Errno. Vet's stdmethods check flags the signature; CI runs with
// -stdmethods=false rather than reshaping an interface we do not own.
func (f *aferoSysFile) Seek(offset int64, whence int) (int64, sys.Errno) {
	position, err := f.file.Seek(offset, whence)
	return position, sys.UnwrapOSError(err)
}

func (f *aferoSysFile) Readdir(count int) ([]sys.Dirent, sys.Errno) {
	infos, err := f.file.Readdir(count)
	if err != nil && err != io.EOF {
		return nil, sys.UnwrapOSError(err)
	}
	entries := make([]sys.Dirent, len(infos))
	for index, info := range infos {
		stat := sys.NewStat_t(info)
		entries[index] = sys.Dirent{Ino: stat.Ino, Name: info.Name(), Type: info.Mode().Type()}
	}
	return entries, 0
}

func (f *aferoSysFile) Write(buffer []byte) (int, sys.Errno) {
	count, err := f.file.Write(buffer)
	return count, sys.UnwrapOSError(err)
}

func (f *aferoSysFile) Pwrite(buffer []byte, offset int64) (int, sys.Errno) {
	count, err := f.file.WriteAt(buffer, offset)
	return count, sys.UnwrapOSError(err)
}

func (f *aferoSysFile) Truncate(size int64) sys.Errno {
	return sys.UnwrapOSError(f.file.Truncate(size))
}

func (f *aferoSysFile) Sync() sys.Errno {
	return sys.UnwrapOSError(f.file.Sync())
}

func (f *aferoSysFile) Datasync() sys.Errno { return f.Sync() }

func (f *aferoSysFile) Utimens(accessNanos, modifyNanos int64) sys.Errno {
	return sys.UnwrapOSError(f.filesystem.Chtimes(
		f.name,
		time.Unix(0, accessNanos),
		time.Unix(0, modifyNanos),
	))
}

func (f *aferoSysFile) Close() sys.Errno {
	return sys.UnwrapOSError(f.file.Close())
}

func aferoName(name string) string {
	if name == "" || name == "." || name == "/" {
		return "."
	}
	return filepath.FromSlash(name)
}
