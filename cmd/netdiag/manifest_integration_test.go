//go:build integration && windows

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Builds the real exe and checks Windows sees the manifest as resource
// RT_MANIFEST #1, and that the exe runs without elevation.
func TestBuiltExe_HasAsInvokerManifestAndRuns(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "netdiag.exe")
	build := exec.Command("go", "build", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	h, err := windows.LoadLibraryEx(exe, 0, windows.LOAD_LIBRARY_AS_DATAFILE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.FreeLibrary(h) }()
	r, err := windows.FindResource(h, windows.ResourceID(1), windows.RT_MANIFEST)
	if err != nil {
		t.Fatalf("no RT_MANIFEST #1: %v", err)
	}
	data, err := windows.LoadResourceData(h, r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`level="asInvoker"`)) {
		t.Errorf("manifest resource lacks asInvoker:\n%s", data)
	}

	out, err := exec.Command(exe).Output()
	if err != nil {
		t.Fatalf("running exe without args: %v", err)
	}
	if !bytes.Contains(out, []byte("Usage:")) {
		t.Errorf("no help output: %q", out)
	}
}
