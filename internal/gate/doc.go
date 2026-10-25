// Package gate implements the dependency layers L1–L5 (SPEC §6): link,
// address, gateway, internet and DNS, with hysteresis, the evidence rules
// that open each layer, skipped reasons for probes above a closed layer,
// and the reset on adapter change.
package gate
