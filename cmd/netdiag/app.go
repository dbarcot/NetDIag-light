package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"netdiag/internal/clock"
	"netdiag/internal/config"
	"netdiag/internal/sim"
	"netdiag/internal/sysapi"
)

// Exit codes shared by all commands (SPEC §8).
const (
	exitOK      = 0
	exitRuntime = 1 // runtime or usage error
	exitConfig  = 2
)

const minHeadlessDuration = 10 * time.Second

// app holds everything main injects, so commands are testable.
type app struct {
	ctx            context.Context // cancelled by Ctrl+C
	clock          clock.Clock
	stdin          io.Reader
	stdout, stderr io.Writer
	console        sysapi.Console
	fs             config.FS
	paths          config.Paths
	version        string
	commit         string
}

// main runs one command and returns the process exit code. After help or
// a failure it waits for Enter when the console belongs to this process
// alone (Explorer double-click), so the window does not vanish.
func (a *app) main(args []string) int {
	code, shownHelp := a.dispatch(args)
	if shownHelp || code != exitOK {
		a.pauseIfOwnConsole()
	}
	return code
}

func (a *app) dispatch(args []string) (code int, shownHelp bool) {
	if len(args) == 0 {
		a.printHelp(a.stdout)
		return exitOK, true
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "help", "-h", "-help", "--help":
		a.printHelp(a.stdout)
		return exitOK, true
	case "version":
		fmt.Fprintf(a.stdout, "netdiag %s (%s, %s)\n", a.version, a.commit, runtime.Version())
		return exitOK, false
	case "save-config":
		return a.saveConfig(rest)
	case "run":
		return a.run(rest)
	default:
		fmt.Fprintf(a.stderr, "netdiag: unknown command %q\n\n", cmd)
		a.printUsage(a.stderr)
		return exitRuntime, false
	}
}

func (a *app) pauseIfOwnConsole() {
	if n, err := a.console.ProcessCount(); err != nil || n != 1 {
		return
	}
	fmt.Fprint(a.stdout, "\nPress Enter to exit...")
	_, _ = bufio.NewReader(a.stdin).ReadString('\n')
}

func (a *app) usageError(format string, args ...any) (int, bool) {
	fmt.Fprintf(a.stderr, "netdiag: "+format+"\n\n", args...)
	a.printUsage(a.stderr)
	return exitRuntime, false
}

func (a *app) saveConfig(args []string) (int, bool) {
	if len(args) == 1 && isHelpArg(args[0]) {
		fmt.Fprint(a.stdout, saveConfigUsage)
		return exitOK, true
	}
	if len(args) > 1 {
		return a.usageError("save-config takes at most one path")
	}
	var path string
	if len(args) == 1 {
		path = args[0]
		if strings.HasPrefix(path, "-") {
			return a.usageError("save-config: unknown flag %s", path)
		}
	}
	written, err := config.Save(a.fs, path, a.paths)
	if err != nil {
		fmt.Fprintf(a.stderr, "netdiag: save-config: %v\n", err)
		return exitRuntime, false
	}
	fmt.Fprintln(a.stdout, written)
	return exitOK, false
}

func isHelpArg(s string) bool {
	return s == "-h" || s == "--help" || s == "-help"
}

// runOptions are the parsed flags of `run`.
type runOptions struct {
	configPath string
	port       int
	portSet    bool
	logPath    string
	csvDir     string
	headless   bool
	duration   time.Duration
	jsonPath   string
	simPath    string // hidden developer flag (SPEC §3)
}

func (a *app) parseRun(args []string) (runOptions, error) {
	var o runOptions
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are printed by the caller with our own usage
	fs.StringVar(&o.configPath, "config", "", "")
	fs.IntVar(&o.port, "port", 0, "")
	fs.StringVar(&o.logPath, "log", "", "")
	fs.StringVar(&o.csvDir, "csv", "", "")
	fs.BoolVar(&o.headless, "headless", false, "")
	fs.DurationVar(&o.duration, "duration", 0, "")
	fs.StringVar(&o.jsonPath, "json", "", "")
	fs.StringVar(&o.simPath, "sim", "", "")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	o.portSet = set["port"]

	switch {
	case fs.NArg() > 0:
		return o, fmt.Errorf("run: unexpected argument %q", fs.Arg(0))
	case o.portSet && (o.port < 1 || o.port > 65535):
		return o, fmt.Errorf("run: --port must be between 1 and 65535")
	case o.headless && o.portSet:
		return o, errors.New("run: --port cannot be used with --headless (no web UI)")
	case o.headless && !set["duration"]:
		return o, errors.New("run: --headless requires --duration")
	case o.headless && o.jsonPath == "":
		return o, errors.New("run: --headless requires --json")
	case o.headless && o.duration < minHeadlessDuration:
		return o, fmt.Errorf("run: --duration must be at least %s", minHeadlessDuration)
	case !o.headless && (set["duration"] || set["json"]):
		return o, errors.New("run: --duration and --json require --headless")
	case o.simPath != "" && o.headless:
		return o, errors.New("run: --sim with --headless is not implemented yet")
	}
	return o, nil
}

// runSim plays a scenario file through the real engine with the plain
// output (SPEC §3 hidden --sim flag). The scenario's config block over the
// built-in defaults is used; --config is not read.
func (a *app) runSim(path string) (int, bool) {
	data, err := a.fs.ReadFile(path)
	if err != nil {
		fmt.Fprintf(a.stderr, "netdiag: --sim: %v\n", err)
		return exitRuntime, false
	}
	sc, err := sim.Parse(data)
	if err != nil {
		fmt.Fprintf(a.stderr, "netdiag: --sim: %v\n", err)
		return exitRuntime, false
	}
	fmt.Fprintf(a.stdout, "netdiag %s\n", a.version)
	if _, err := sim.Live(a.ctx, sc, a.clock, a.stdout); err != nil {
		fmt.Fprintf(a.stderr, "netdiag: --sim: %v\n", err)
		return exitRuntime, false
	}
	return exitOK, false
}

func (a *app) run(args []string) (int, bool) {
	o, err := a.parseRun(args)
	if errors.Is(err, flag.ErrHelp) {
		a.printHelp(a.stdout)
		return exitOK, true
	}
	if err != nil {
		msg := err.Error()
		if !strings.HasPrefix(msg, "run:") {
			msg = "run: " + msg
		}
		return a.usageError("%s", msg)
	}
	if o.simPath != "" {
		return a.runSim(o.simPath)
	}

	cfg, source, err := config.Load(a.fs, o.configPath, a.paths)
	if err != nil {
		a.printConfigError(source, err)
		return exitConfig, false
	}
	if o.portSet {
		cfg.UI.Port = o.port
	}
	if o.csvDir != "" {
		cfg.CSV.Dir = o.csvDir
	}

	fmt.Fprintf(a.stdout, "netdiag %s\nConfig: %s\n", a.version, source)
	fmt.Fprintln(a.stdout, "Config OK. Probing is not implemented in this build yet.")
	return exitOK, false
}

func (a *app) printConfigError(source string, err error) {
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		for _, f := range ve.Fields {
			fmt.Fprintf(a.stderr, "netdiag: config error in %s: %v\n", source, f)
		}
		return
	}
	fmt.Fprintf(a.stderr, "netdiag: config error in %s: %v\n", source, err)
}
