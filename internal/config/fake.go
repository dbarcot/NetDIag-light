package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// FakeFS is an in-memory FS for tests. Paths are compared after
// filepath.Clean. Directories are implicit; ReadOnly marks a directory in
// which CreateTemp fails with fs.ErrPermission.
type FakeFS struct {
	mu         sync.Mutex
	Files      map[string][]byte
	ReadOnly   map[string]bool
	Dirs       map[string]bool // directories created with MkdirAll
	FailWrite  bool            // TempFile.Write fails
	FailRename bool            // RenameNoReplace fails
	// BeforeRename, if set, runs at the start of RenameNoReplace (without
	// the lock held), e.g. to create the target and simulate a race.
	BeforeRename func()
	seq          int
}

// NewFakeFS returns an empty FakeFS.
func NewFakeFS() *FakeFS {
	return &FakeFS{Files: map[string][]byte{}, ReadOnly: map[string]bool{}, Dirs: map[string]bool{}}
}

// Put stores a file.
func (f *FakeFS) Put(name string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Files[filepath.Clean(name)] = data
}

// Get returns a file's content and whether it exists.
func (f *FakeFS) Get(name string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.Files[filepath.Clean(name)]
	return b, ok
}

func (f *FakeFS) ReadFile(name string) ([]byte, error) {
	b, ok := f.Get(name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), b...), nil
}

func (f *FakeFS) Stat(name string) (fs.FileInfo, error) {
	b, ok := f.Get(name)
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fakeInfo{name: filepath.Base(name), size: int64(len(b))}, nil
}

func (f *FakeFS) MkdirAll(path string, _ fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Dirs[filepath.Clean(path)] = true
	return nil
}

func (f *FakeFS) CreateTemp(dir, pattern string) (TempFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir = filepath.Clean(dir)
	if f.ReadOnly[dir] {
		return nil, &fs.PathError{Op: "createtemp", Path: dir, Err: fs.ErrPermission}
	}
	f.seq++
	name := filepath.Join(dir, fmt.Sprintf("%s%d", pattern, f.seq))
	f.Files[name] = nil
	return &fakeTemp{fs: f, name: name}, nil
}

func (f *FakeFS) RenameNoReplace(oldpath, newpath string) error {
	if f.BeforeRename != nil {
		f.BeforeRename()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailRename {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: errors.New("injected rename failure")}
	}
	oldpath, newpath = filepath.Clean(oldpath), filepath.Clean(newpath)
	if _, exists := f.Files[newpath]; exists {
		return &fs.PathError{Op: "rename", Path: newpath, Err: fs.ErrExist}
	}
	b, ok := f.Files[oldpath]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	delete(f.Files, oldpath)
	f.Files[newpath] = b
	return nil
}

func (f *FakeFS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	name = filepath.Clean(name)
	if _, ok := f.Files[name]; !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	delete(f.Files, name)
	return nil
}

type fakeTemp struct {
	fs   *FakeFS
	name string
}

func (t *fakeTemp) Name() string { return t.name }
func (t *fakeTemp) Close() error { return nil }

func (t *fakeTemp) Write(p []byte) (int, error) {
	t.fs.mu.Lock()
	defer t.fs.mu.Unlock()
	if t.fs.FailWrite {
		return 0, errors.New("injected write failure")
	}
	t.fs.Files[t.name] = append(t.fs.Files[t.name], p...)
	return len(p), nil
}

type fakeInfo struct {
	name string
	size int64
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() fs.FileMode  { return 0o644 }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return false }
func (i fakeInfo) Sys() any           { return nil }
