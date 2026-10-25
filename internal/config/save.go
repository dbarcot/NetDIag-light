package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// ErrExists is returned by Save when the target file already exists.
var ErrExists = errors.New("file already exists")

// errNotWritable marks a directory in which no file could be created.
var errNotWritable = errors.New("directory not writable")

// Save writes the full default config (every key expanded) and returns the
// path written. With an explicit path it writes only there. Otherwise it
// writes netdiag.json next to the exe, falling back to
// %APPDATA%\netdiag\netdiag.json when the exe directory is not writable.
// It never overwrites an existing file and writes atomically.
func Save(fsys FS, explicit string, paths Paths) (string, error) {
	data, err := Default().Marshal()
	if err != nil {
		return "", err
	}
	if explicit != "" {
		return explicit, saveTo(fsys, explicit, data)
	}
	primary, fallback := paths.exeFile(), paths.appDataFile()
	if primary == "" && fallback == "" {
		return "", errors.New("cannot determine where to save: exe directory and %APPDATA% are unknown")
	}
	if primary != "" {
		err := saveTo(fsys, primary, data)
		if !errors.Is(err, errNotWritable) || fallback == "" {
			return primary, err
		}
	}
	if err := fsys.MkdirAll(filepath.Dir(fallback), 0o755); err != nil {
		return fallback, err
	}
	return fallback, saveTo(fsys, fallback, data)
}

func saveTo(fsys FS, path string, data []byte) error {
	_, err := fsys.Stat(path)
	switch {
	case err == nil:
		return fmt.Errorf("refusing to overwrite %s: %w", path, ErrExists)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return writeAtomic(fsys, path, data)
}

// writeAtomic writes to a temp file in the same directory and renames it
// into place; the temp file is removed on any failure.
func writeAtomic(fsys FS, path string, data []byte) (err error) {
	tmp, err := fsys.CreateTemp(filepath.Dir(path), ".netdiag-*.tmp")
	if err != nil {
		return fmt.Errorf("%w: %w", errNotWritable, err)
	}
	defer func() {
		if err != nil {
			_ = fsys.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return fsys.Rename(tmp.Name(), path)
}
