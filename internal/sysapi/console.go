// Package sysapi defines interfaces for OS information and console access,
// with Windows implementations and fakes for tests.
package sysapi

// Console is access to the process's console. Phase 1 extends it with mode
// handling and raw key input.
type Console interface {
	// ProcessCount returns how many processes are attached to the console.
	// 1 means the console was created for this process alone, e.g. by a
	// double-click in Explorer. An error means there is no console.
	ProcessCount() (int, error)
}
