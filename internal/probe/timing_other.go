//go:build !windows

package probe

import "time"

// stopwatch returns a function reporting the time since the call.
func stopwatch() func() time.Duration {
	start := time.Now()
	return func() time.Duration { return time.Since(start) }
}
