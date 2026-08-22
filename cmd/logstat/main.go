// Command logstat counts code words in a line-oriented log file and keeps
// incremental counters in Redis.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/snmp161/logstat/internal/config"
	"github.com/snmp161/logstat/internal/daemon"
	"github.com/snmp161/logstat/internal/lockfile"
	"github.com/snmp161/logstat/internal/logging"
	"github.com/snmp161/logstat/internal/store"
)

// Injected at build time via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `logstat — count code words in a log file and keep the counters in Redis.

Usage:
  logstat run   --config <path>              run the daemon (main mode)
  logstat clear --config <path> --all        zero all counters of the config
  logstat clear --config <path> --action <w> zero a single counter
  logstat version                            print version, commit and build date
  logstat --help                             this help

Run "logstat <command> --help" for the flags of a command.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "clear":
		return cmdClear(args[1:])
	case "version":
		fmt.Printf("logstat %s (commit %s, built %s)\n", version, commit, date)
		return 0
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// configFlags registers --config/-c on fs and returns a pointer to its value.
func configFlags(fs *flag.FlagSet) *string {
	var path string
	fs.StringVar(&path, "config", "", "path to the YAML configuration file")
	fs.StringVar(&path, "c", "", "path to the YAML configuration file (shorthand)")
	return &path
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := configFlags(fs)
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	cfg, lg, code := setup(fs, *cfgPath)
	if cfg == nil {
		return code
	}
	defer func() { _ = lg.Close() }()

	if err := cfg.CheckPaths(); err != nil {
		lg.Error("configuration error", "error", err)
		return 1
	}

	lock, err := lockfile.Acquire(cfg.LockFile)
	if err != nil {
		lg.Error("cannot acquire lock, is another instance running?", "error", err)
		return 1
	}
	defer func() { _ = lock.Release() }()

	st := store.New(cfg.Redis, store.ShortHostname(), cfg.HeartbeatKey)
	defer func() { _ = st.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP reopens the daemon's own log file after an external logrotate run.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := lg.Reopen(); err != nil {
				lg.Error("cannot reopen log file", "error", err)
				continue
			}
			lg.Info("log file reopened on SIGHUP")
		}
	}()

	if err := daemon.New(cfg, lg.Logger, st, daemon.WithVersion(version)).Run(ctx); err != nil {
		lg.Error("daemon failed", "error", err)
		return 1
	}
	return 0
}

func cmdClear(args []string) int {
	fs := flag.NewFlagSet("clear", flag.ContinueOnError)
	cfgPath := configFlags(fs)
	action := fs.String("action", "", "zero only this action")
	all := fs.Bool("all", false, "zero every action of the configuration")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	cfg, lg, code := setup(fs, *cfgPath)
	if cfg == nil {
		return code
	}
	defer func() { _ = lg.Close() }()

	switch {
	case *all && *action != "":
		lg.Error("--all and --action are mutually exclusive")
		return 2
	case !*all && *action == "":
		lg.Error("specify either --all or --action <word>")
		return 2
	}

	actions := cfg.Actions
	if *action != "" {
		if !slices.Contains(cfg.Actions, *action) {
			lg.Error("action is not listed in the configuration", "action", *action, "configured", cfg.Actions)
			return 2
		}
		actions = []string{*action}
	}

	st := store.New(cfg.Redis, store.ShortHostname(), cfg.HeartbeatKey)
	defer func() { _ = st.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now()
	failed := 0
	for _, a := range actions {
		if err := st.Reset(ctx, a, now); err != nil {
			lg.Error("cannot reset counter", "action", a, "error", err)
			failed++
			continue
		}
		lg.Info("counter cleared", "action", a, "host", st.Host(), "heartbeat", st.HeartbeatEnabled())
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// parseExit maps a flag parsing outcome to an exit code: asking for help is a
// success, anything else is a usage error.
func parseExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// setup loads the configuration and builds the logger. On failure it reports
// the problem on stderr and returns a nil config together with the exit code.
func setup(fs *flag.FlagSet, cfgPath string) (*config.Config, *logging.Logger, int) {
	if cfgPath == "" {
		fmt.Fprintf(os.Stderr, "--config is required\n\n")
		fs.Usage()
		return nil, nil, 2
	}

	cfg, warnings, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return nil, nil, 1
	}

	lg, err := logging.New(cfg.Logging)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot initialise logging: %v\n", err)
		return nil, nil, 1
	}
	// go-redis logs pool problems on its own; route them into our log at debug
	// level so that stderr stays clean and an outage is reported once.
	store.SetLibraryLogger(lg.Logger)

	lg.Info("configuration loaded", "path", cfgPath, "version", version)
	for _, w := range warnings {
		lg.Warn(w)
	}
	return cfg, lg, 0
}
