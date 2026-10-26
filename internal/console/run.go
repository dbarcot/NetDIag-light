package console

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"
	"unicode"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/engine"
	"netdiag/internal/store"
	"netdiag/internal/sysapi"
)

// VT sequences of the live view.
const (
	vtHome       = "\x1b[H"
	vtClear      = "\x1b[2J"
	vtEraseLine  = "\x1b[K"
	vtEraseBelow = "\x1b[J"
	vtHideCursor = "\x1b[?25l"
	vtShowCursor = "\x1b[?25h"
)

// DefaultSummaryEvery is the plain-mode summary period (SPEC §9).
const DefaultSummaryEvery = 30 * time.Second

// plainWidth is the table width of plain summaries (no console to measure).
const plainWidth = 120

// Source is what the console renders: the engine.
type Source interface {
	Snapshot() engine.Snapshot
	Store() *store.Store
	Reset()
}

// Options configure Run.
type Options struct {
	Console sysapi.Console
	// Stdout receives the output when stdout is not a console (UTF-8).
	Stdout io.Writer
	Clock  clock.Clock
	// LookupEnv reads NO_COLOR (os.LookupEnv).
	LookupEnv func(string) (string, bool)
	Config    *config.Config
	Meta      Meta
	// Location of the timestamps; nil is time.Local.
	Location *time.Location
	// SummaryEvery is the plain-mode summary period; 0 is 30 s.
	SummaryEvery time.Duration
	// Logger receives internal errors (panics with stacks); nil discards.
	Logger *slog.Logger
}

// ShutdownGrace bounds the wait for work after a stop request (q, Ctrl+C,
// console close): a probe stuck in an OS call that ignores cancellation
// must not hang the exit. Whatever still runs dies with the process.
const ShutdownGrace = 3 * time.Second

// Run owns the console while work runs: it saves and changes the console
// modes, starts work in its own goroutine, renders the live view at 1 Hz
// (or the plain output when stdout is not a VT console) and reads keys:
// `q` cancels, `r` resets the counters. It returns when work returns, ctx
// ends or `q` is pressed: after a stop it shows "Stopping…" and waits up to
// ShutdownGrace for work, then prints the final summary. The original
// console modes are restored on every exit path, including a panic in the
// render loop (restored before re-panicking). A panic in work is returned
// as an "internal error"; a panic in the key reader only stops key
// handling. It returns the error of work (nil when work did not stop in
// time).
func Run(ctx context.Context, o Options, src Source, work func(ctx context.Context) error) (err error) {
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.SummaryEvery <= 0 {
		o.SummaryEvery = DefaultSummaryEvery
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	sess := sysapi.EnterRaw(o.Console)
	u := &ui{o: o, src: src, tty: sess.VT, keys: sess.Keys, w: o.Stdout}
	if _, e := o.Console.OutMode(); e == nil {
		u.w = o.Console // a console: write UTF-16 (WriteConsoleW)
	}
	if u.tty {
		_, noColor := o.LookupEnv("NO_COLOR")
		u.color = !noColor
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workDone := make(chan error, 1) // buffered: a late return never blocks
	go func() {
		defer cancel()
		defer func() {
			if p := recover(); p != nil {
				u.logPanic("work", p)
				workDone <- fmt.Errorf("internal error: %v", p)
			}
		}()
		workDone <- work(ctx)
	}()
	defer func() {
		if p := recover(); p != nil {
			cancel()
			u.leave()
			_ = sess.Restore()
			panic(p)
		}
	}()

	redraw := make(chan struct{}, 1)
	if u.keys {
		go u.readKeys(ctx, cancel, redraw)
	}
	u.loop(ctx, redraw)
	err = u.waitWork(workDone)
	u.leave()
	if err == nil {
		u.finalSummary()
	}
	if rerr := sess.Restore(); rerr != nil && err == nil {
		err = fmt.Errorf("restore console modes: %w", rerr)
	}
	return err
}

// waitWork shows "Stopping…" and waits for work at most ShutdownGrace.
func (u *ui) waitWork(workDone <-chan error) error {
	select {
	case err := <-workDone: // work ended by itself (end of a simulation, fatal error)
		return err
	default:
	}
	if u.tty {
		u.stopping = true
		u.draw()
	}
	t := u.o.Clock.NewTimer(ShutdownGrace)
	defer t.Stop()
	select {
	case err := <-workDone:
		return err
	case <-t.C():
		u.o.Logger.Warn("work did not stop in time; exiting anyway", "grace", ShutdownGrace)
		return nil
	}
}

func (u *ui) logPanic(where string, p any) {
	u.o.Logger.Error("panic", "in", where, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
}

type ui struct {
	o         Options
	src       Source
	tty, keys bool
	color     bool
	w         io.Writer
	drawn     bool // the live view is on screen
	stopping  bool // a stop was requested: the footer says "Stopping…"
	lastLine  string
	lastSeq   int // Seq of the newest adapter event printed
}

func (u *ui) write(s string) { _, _ = io.WriteString(u.w, s) }

func (u *ui) loop(ctx context.Context, redraw <-chan struct{}) {
	tk := u.o.Clock.NewTicker(time.Second)
	defer tk.Stop()
	nextSummary := u.o.Clock.Now().Add(u.o.SummaryEvery)
	if u.tty {
		u.write(vtHideCursor + vtClear)
		u.draw()
	}
	for {
		var now time.Time
		select {
		case <-ctx.Done():
			return
		case <-redraw:
			now = u.o.Clock.Now()
		case now = <-tk.C():
		}
		if u.tty {
			u.draw()
			continue
		}
		snap := u.src.Snapshot()
		u.plainChanges(snap)
		if !now.Before(nextSummary) { // a fixed 30 s grid
			u.write(u.summary(snap, "summary", plainWidth, false))
			for !nextSummary.After(now) {
				nextSummary = nextSummary.Add(u.o.SummaryEvery)
			}
		}
	}
}

func (u *ui) view(snap engine.Snapshot) View {
	v := BuildView(snap, u.src.Store(), u.o.Config, u.o.Meta)
	v.Keys, v.Stopping = u.keys, u.stopping
	return v
}

// consoleWidth is the console width, or fallback when it cannot be read.
func (u *ui) consoleWidth(fallback int) int {
	if w, err := u.o.Console.Width(); err == nil && w > 1 {
		return w
	}
	return fallback
}

// draw writes one frame over the previous one: cursor home, every line
// erased to its end, everything below erased (no full clear, no flicker).
func (u *ui) draw() {
	width := u.consoleWidth(80)
	// one column spare: a full line would leave the cursor in the wrap state
	lines := Render(u.view(u.src.Snapshot()), width-1, u.o.Location)
	if h, err := u.o.Console.Height(); err == nil && h > 0 {
		lines = fitHeight(lines, h, width-1)
	}
	var b strings.Builder
	b.WriteString(vtHome)
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l.String(u.color) + vtEraseLine)
	}
	b.WriteString(vtEraseBelow)
	u.write(b.String())
	u.drawn = true
}

// fitHeight cuts a frame taller than the window: homing the cursor only
// overwrites the frame if it never scrolls. The last row says how much
// is hidden.
func fitHeight(lines []line, height, width int) []line {
	if len(lines) <= height {
		return lines
	}
	if height < 2 {
		return lines[:max(height, 0)]
	}
	hidden := len(lines) - (height - 1)
	more := line{{fmt.Sprintf("… %d more lines: enlarge the window", hidden), sgrGrey}}.fit(width)
	return append(lines[:height-1:height-1], more)
}

// leave puts the cursor below the drawn view and shows it again.
func (u *ui) leave() {
	if u.tty && u.drawn {
		u.write("\r\n" + vtShowCursor)
		u.drawn = false
	} else if u.tty {
		u.write(vtShowCursor)
	}
}

func (u *ui) stamp(t time.Time) string { return t.In(u.o.Location).Format("15:04:05") }

// plainChanges prints adapter events not printed yet (by Seq, so a capped
// event list never repeats or skips a printed one) and the verdict line
// when the verdict or the flags changed. Text goes through clean.
func (u *ui) plainChanges(snap engine.Snapshot) {
	for _, ev := range snap.AdapterEvents {
		if ev.Seq <= u.lastSeq {
			continue
		}
		u.write(fmt.Sprintf("%s  %s\n", u.stamp(ev.Time), clean(ev.String())))
		u.lastSeq = ev.Seq
	}
	if l := clean(engine.Describe(snap)); l != u.lastLine {
		u.write(fmt.Sprintf("%s  %s\n", u.stamp(snap.Time), l))
		u.lastLine = l
	}
}

// summaryIndent prefixes the lines of a summary block below its title.
const summaryIndent = "    "

// summary is the plain block: verdict, call quality, Teams/Meet and the
// targets table without sparklines, the table width wide. With fitTitle
// the title line is cut to the block width too (a console).
func (u *ui) summary(snap engine.Snapshot, title string, width int, fitTitle bool) string {
	v := u.view(snap)
	v.Sparkline = false
	var b strings.Builder
	head := plain(fmt.Sprintf("%s  %s: %s", u.stamp(snap.Time), title, clean(engine.Describe(snap))))
	if fitTitle {
		head = head.fit(width + len(summaryIndent))
	}
	b.WriteString(head.String(false) + "\n")
	for _, l := range append([]line{qualityLine(v, width).fit(width), plain(v.Teams).fit(width)}, targetTable(v, width)...) {
		b.WriteString(summaryIndent + strings.TrimRight(l.String(false), " ") + "\n")
	}
	return b.String()
}

// finalWidthMax bounds the final summary table on a wide console.
const finalWidthMax = 200

func (u *ui) finalSummary() {
	snap := u.src.Snapshot()
	if !u.tty {
		u.plainChanges(snap)
		u.write(u.summary(snap, "final", plainWidth, false))
		return
	}
	width := plainWidth
	if w := u.consoleWidth(0); w > 0 {
		// indent plus one column spare: no line reaches the last column
		width = min(w-len(summaryIndent)-1, finalWidthMax)
	}
	u.write(u.summary(snap, "final", width, true))
}

// readKeys handles q (cancel) and r (reset counters, redraw) until ctx
// ends or the console stops delivering keys.
func (u *ui) readKeys(ctx context.Context, cancel context.CancelFunc, redraw chan<- struct{}) {
	defer func() {
		if p := recover(); p != nil {
			u.logPanic("key reader", p) // keys stop; Ctrl+C still works
		}
	}()
	for {
		k, err := u.o.Console.ReadKey(ctx)
		if err != nil {
			return // ctx ended, or key input broke: Ctrl+C still works
		}
		switch unicode.ToLower(k.Rune) {
		case 'q':
			cancel()
			return
		case 'r':
			u.src.Reset()
			select {
			case redraw <- struct{}{}:
			default:
			}
		}
	}
}
