package sim

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/verdict"
)

// Live runs the real scheduler with random offsets: the scenario's state
// must still come out right (a 40 s scenario at 100× is 0.4 s).
func TestLive_ReachesScenarioVerdicts(t *testing.T) {
	t.Parallel()
	tests := map[string]verdict.Verdict{
		"isp_down": verdict.ISPDown,
		"no_link":  verdict.NoLink,
		"ok":       verdict.OK,
	}
	scs := loadScenarios(t)
	for name, want := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			snap, err := Live(context.Background(), scs[name], clock.NewScaled(100), &out)
			if err != nil {
				t.Fatal(err)
			}
			if snap.Result.Verdict != want {
				t.Errorf("final verdict %s, want %s\n%s", snap.Result.Verdict, want, out.String())
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			if !strings.HasPrefix(lines[0], "simulation \""+name+"\"") {
				t.Errorf("header %q", lines[0])
			}
			if lastLine := lines[len(lines)-1]; !strings.Contains(lastLine, "end of simulation: "+string(want)) {
				t.Errorf("last line %q", lastLine)
			}
		})
	}
}

func TestLive_PrintsChangesOnly(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if _, err := Live(context.Background(), loadScenarios(t)["isp_down"], clock.NewScaled(100), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Count(text, "ISP_DOWN") != 2 { // the change line and the end line
		t.Errorf("want one change line plus the end line for ISP_DOWN:\n%s", text)
	}
	if !strings.Contains(text, "OK [exit 0]") || !strings.Contains(text, "ISP_DOWN (internet down) [exit 15]") {
		t.Errorf("output:\n%s", text)
	}
}

func TestLive_CancelStopsEverything(t *testing.T) {
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = Live(ctx, loadScenarios(t)["gw_down"], clock.NewScaled(10), &bytes.Buffer{})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Live did not stop within 2 s of cancel")
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("goroutine leak: %d before, %d after", before, n)
	}
}
