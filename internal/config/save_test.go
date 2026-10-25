package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var testPaths = Paths{ExeDir: `C:\tools`, AppData: `C:\Users\u\AppData\Roaming`}

func TestSave_WritesDefaultsThatRoundTrip(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	path, err := Save(fsys, "", testPaths)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(testPaths.ExeDir, FileName); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, _ := fsys.Get(path)
	c, err := Parse(data)
	if err != nil {
		t.Fatalf("saved file does not load: %v", err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Error("saved config differs from defaults")
	}
	// Every key expanded: the file must name each top-level and nested key.
	for _, key := range []string{`"rates"`, `"src_ports"`, `"timeouts"`, `"udp_ports"`, `"max_mb"`, `"expect_body"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("saved file lacks %s", key)
		}
	}
	assertNoTempFiles(t, fsys)
}

func TestSave_RefusesToOverwrite(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	target := filepath.Join(testPaths.ExeDir, FileName)
	fsys.Put(target, []byte("keep"))
	_, err := Save(fsys, "", testPaths)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if b, _ := fsys.Get(target); string(b) != "keep" {
		t.Error("existing file was modified")
	}
	if _, ok := fsys.Get(filepath.Join(testPaths.AppData, "netdiag", FileName)); ok {
		t.Error("must not fall back to %APPDATA% when the primary file exists")
	}
}

func TestSave_ExplicitPath(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	p := `D:\cfg\x.json`
	got, err := Save(fsys, p, testPaths)
	if err != nil || got != p {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := Save(fsys, p, testPaths); !errors.Is(err, ErrExists) {
		t.Errorf("second save: want ErrExists, got %v", err)
	}
}

func TestSave_ExplicitPathNotWritableHasNoFallback(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	fsys.ReadOnly[`D:\ro`] = true
	if _, err := Save(fsys, `D:\ro\x.json`, testPaths); err == nil {
		t.Fatal("want error")
	}
	if len(fsys.Files) != 0 {
		t.Errorf("unexpected files: %v", fsys.Files)
	}
}

func TestSave_FallsBackToAppDataWhenExeDirNotWritable(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	fsys.ReadOnly[testPaths.ExeDir] = true
	path, err := Save(fsys, "", testPaths)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(testPaths.AppData, "netdiag", FileName)
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if !fsys.Dirs[filepath.Dir(want)] {
		t.Error("%APPDATA%\\netdiag was not created")
	}
	if _, ok := fsys.Get(want); !ok {
		t.Error("fallback file not written")
	}
}

func TestSave_NoFallbackWithoutAppData(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	fsys.ReadOnly[testPaths.ExeDir] = true
	if _, err := Save(fsys, "", Paths{ExeDir: testPaths.ExeDir}); err == nil {
		t.Fatal("want error when exe dir is read-only and APPDATA unknown")
	}
}

func TestSave_TempFileRemovedOnFailure(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"write", "rename"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fsys := NewFakeFS()
			fsys.FailWrite = mode == "write"
			fsys.FailRename = mode == "rename"
			if _, err := Save(fsys, "", testPaths); err == nil {
				t.Fatal("want error")
			}
			if len(fsys.Files) != 0 {
				t.Errorf("files left behind: %v", fsys.Files)
			}
		})
	}
}

func TestSave_RealFilesystem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, err := Save(OSFS{}, "", Paths{ExeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	c, src, err := Load(OSFS{}, path, Paths{})
	if err != nil || src != path || !reflect.DeepEqual(c, Default()) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := Save(OSFS{}, "", Paths{ExeDir: dir}); !errors.Is(err, ErrExists) {
		t.Errorf("want ErrExists, got %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("want only %s in dir, got %d entries", FileName, len(entries))
	}
}

func TestSave_TargetCreatedBeforeRenameIsNotOverwritten(t *testing.T) {
	t.Parallel()
	fsys := NewFakeFS()
	target := filepath.Join(testPaths.ExeDir, FileName)
	fsys.BeforeRename = func() { fsys.Put(target, []byte("other process")) }
	_, err := Save(fsys, "", testPaths)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if b, _ := fsys.Get(target); string(b) != "other process" {
		t.Errorf("target overwritten: %q", b)
	}
	if len(fsys.Files) != 1 {
		t.Errorf("temp file left: %v", fsys.Files)
	}
}

// racingOSFS creates the target right before the real non-replacing move.
type racingOSFS struct {
	OSFS
	content []byte
}

func (r racingOSFS) RenameNoReplace(oldpath, newpath string) error {
	if err := os.WriteFile(newpath, r.content, 0o644); err != nil {
		return err
	}
	return r.OSFS.RenameNoReplace(oldpath, newpath)
}

func TestSave_RealFilesystemRaceNeverOverwrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, FileName)
	_, err := Save(racingOSFS{content: []byte("other process")}, target, Paths{})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "other process" {
		t.Errorf("target overwritten: %q", b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temp file left: %d entries", len(entries))
	}
}

func TestOSFS_RenameNoReplace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (OSFS{}).RenameNoReplace(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("want fs.ErrExist, got %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Errorf("target replaced: %q", b)
	}
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := (OSFS{}).RenameNoReplace(src, dst); err != nil {
		t.Fatalf("move to free name: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("moved content = %q", b)
	}
}

func assertNoTempFiles(t *testing.T, fsys *FakeFS) {
	t.Helper()
	for name := range fsys.Files {
		if strings.Contains(filepath.Base(name), ".tmp") {
			t.Errorf("temp file left: %s", name)
		}
	}
}
