package console

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"netdiag/internal/engine"
	"netdiag/internal/sim"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden/console")

var goldenDir = filepath.Join("..", "..", "testdata", "golden", "console")

// goldenScenarios are rendered at a fixed second of each scenario.
var goldenScenarios = []struct {
	name string
	at   int
}{
	{"ok", 30},
	{"gw_down", 30},
	{"isp_down", 30},
	{"captive_portal", 30},
	{"lan_loss", 40},
	{"poor_voip", 40},
	{"no_media_fallback", 30},
	{"teams_partial_3480", 30},
	{"teams_relay_unreachable", 30},
	{"wifi_ssid_hidden", 20},
	{"cpu_high", 30},
}

var goldenWidths = []int{80, 120, 200}

func loadScenario(t *testing.T, name string) *sim.Scenario {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scenarios", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	sc, err := sim.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// viewAt plays a scenario to second at and builds the console view.
func viewAt(t *testing.T, name string, at int) View {
	t.Helper()
	sc := loadScenario(t, name)
	var v View
	var got bool
	err := sim.Play(sc, func(sec int, eng *engine.Engine) bool {
		if sec < at {
			return true
		}
		v = BuildView(eng.Snapshot(), eng.Store(), sc.Config(), Meta{
			Version: "1.2.3", ConfigSource: "simulation " + name, WebURL: "http://127.0.0.1:48765/",
		})
		got = true
		return false
	})
	if err != nil || !got {
		t.Fatalf("play %s to %d s: %v (reached: %v)", name, at, err, got)
	}
	v.Keys = true
	return v
}

func renderText(v View, width int, color bool) string {
	var b strings.Builder
	for _, l := range Render(v, width, time.UTC) {
		b.WriteString(l.String(color) + "\n")
	}
	return b.String()
}

func TestGolden(t *testing.T) {
	t.Parallel()
	for _, g := range goldenScenarios {
		v := viewAt(t, g.name, g.at)
		for _, w := range goldenWidths {
			for _, color := range []bool{false, true} {
				suffix := ".txt"
				if color {
					suffix = "_ansi.txt"
				}
				file := filepath.Join(goldenDir, fmt.Sprintf("%s_%d%s", g.name, w, suffix))
				got := renderText(v, w, color)
				if *update {
					if err := os.MkdirAll(goldenDir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(file)
				if err != nil {
					t.Fatalf("%v (run go test ./internal/console -update)", err)
				}
				if got != string(want) {
					t.Errorf("%s differs from the golden file:\n%s", file, got)
				}
			}
		}
	}
}

// Every line of every golden frame fits its width.
func TestGolden_LinesFitWidth(t *testing.T) {
	t.Parallel()
	for _, g := range goldenScenarios {
		v := viewAt(t, g.name, g.at)
		for _, w := range append(goldenWidths, 40, 60) {
			for i, l := range Render(v, w, time.UTC) {
				if n := l.width(); n > w {
					t.Errorf("%s at %d: line %d is %d wide: %q", g.name, w, i, n, l.String(false))
				}
				if n := utf8.RuneCountInString(l.String(false)); n != l.width() {
					t.Errorf("width mismatch %d vs %d", n, l.width())
				}
			}
		}
	}
}
