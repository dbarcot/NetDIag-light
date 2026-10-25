//go:build integration && windows

package sysapi

import "testing"

func TestWinConsole_ProcessCount(t *testing.T) {
	n, err := NewConsole().ProcessCount()
	if err != nil {
		t.Skipf("no console attached to the test process: %v", err)
	}
	// go test runs the binary under the go tool, so it never owns the console alone.
	if n < 2 {
		t.Errorf("ProcessCount = %d, want >= 2 under go test", n)
	}
}
