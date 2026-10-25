// Package sim is the simulation mode: scripted scenarios
// (testdata/scenarios/*.json, format in docs/TESTING.md §3) drive fake
// system state and fake probe results through the real engine, gates and
// verdicts. Scenarios run deterministically second by second for tests
// (Run) or in real time for demos and UI tests (Live).
package sim
