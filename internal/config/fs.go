package config

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// FS is the filesystem access config needs; OSFS is the real one and
// FakeFS the in-memory one for tests.
type FS interface {
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
	MkdirAll(path string, perm fs.FileMode) error
	CreateTemp(dir, pattern string) (TempFile, error)
	// RenameNoReplace moves oldpath to newpath and fails with an error
	// matching fs.ErrExist if newpath already exists.
	RenameNoReplace(oldpath, newpath string) error
	Remove(name string) error
}

// TempFile is a file created by FS.CreateTemp.
type TempFile interface {
	io.Writer
	Close() error
	Name() string
}

// OSFS implements FS with the os package and MoveFileEx.
type OSFS struct{}

func (OSFS) ReadFile(name string) ([]byte, error)         { return os.ReadFile(name) }
func (OSFS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (OSFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (OSFS) Remove(name string) error                     { return os.Remove(name) }
func (OSFS) CreateTemp(dir, pattern string) (TempFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// RenameNoReplace uses MoveFileEx without MOVEFILE_REPLACE_EXISTING;
// os.Rename on Windows would replace an existing target.
func (OSFS) RenameNoReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	err = windows.MoveFileEx(from, to, 0)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		err = fs.ErrExist
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
