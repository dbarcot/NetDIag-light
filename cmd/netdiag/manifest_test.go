package main

import (
	"bytes"
	"os"
	"testing"
)

// The committed .syso must contain the current manifest; otherwise it was
// not regenerated with `go generate ./cmd/netdiag`.
func TestManifestSyso_MatchesManifest(t *testing.T) {
	t.Parallel()
	manifest, err := os.ReadFile("netdiag.manifest")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifest, []byte(`level="asInvoker"`)) {
		t.Error(`manifest must request level="asInvoker"`)
	}
	syso, err := os.ReadFile("rsrc_windows_amd64.syso")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(syso, manifest) {
		t.Error("rsrc_windows_amd64.syso is stale: run go generate ./cmd/netdiag")
	}
}
