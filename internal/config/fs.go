package config

import (
	"io"
	"io/fs"
	"os"
)

// FS is the filesystem access config needs; OSFS is the real one and
// FakeFS the in-memory one for tests.
type FS interface {
	ReadFile(name string) ([]byte, error)
	Stat(name string) (fs.FileInfo, error)
	MkdirAll(path string, perm fs.FileMode) error
	CreateTemp(dir, pattern string) (TempFile, error)
	Rename(oldpath, newpath string) error
	Remove(name string) error
}

// TempFile is a file created by FS.CreateTemp.
type TempFile interface {
	io.Writer
	Close() error
	Name() string
}

// OSFS implements FS with the os package.
type OSFS struct{}

func (OSFS) ReadFile(name string) ([]byte, error)         { return os.ReadFile(name) }
func (OSFS) Stat(name string) (fs.FileInfo, error)        { return os.Stat(name) }
func (OSFS) MkdirAll(path string, perm fs.FileMode) error { return os.MkdirAll(path, perm) }
func (OSFS) Rename(oldpath, newpath string) error         { return os.Rename(oldpath, newpath) }
func (OSFS) Remove(name string) error                     { return os.Remove(name) }
func (OSFS) CreateTemp(dir, pattern string) (TempFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}
