// Command mergeyard turns ready GitHub issues into reviewed pull requests.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
)

// command is one entry in the PRD §29 command table.
type command struct {
	name    string // one or more words, e.g. "repo add"
	args    string // required positional placeholders, e.g. "<run-id>"
	summary string
}

func (c command) usage() string {
	if c.args == "" {
		return c.name
	}
	return c.name + " " + c.args
}

func (c command) arity() int {
	return len(strings.Fields(c.args))
}

var commands = []command{
	{name: "start", summary: "start the scheduler and dashboard (default)"},
	{name: "init", summary: "interactive setup: writes config, checks tools"},
	{name: "repo add", args: "<owner/repo>", summary: "add a repository to the config"},
	{name: "status", summary: "show scheduler and run status"},
	{name: "doctor", summary: "check tools, auth, and config"},
	{name: "open", summary: "open the dashboard in a browser"},
	{name: "pause", summary: "stop claiming new issues"},
	{name: "resume", summary: "resume claiming new issues"},
	{name: "watch", args: "<run-id>", summary: "attach read-only to a run's session"},
	{name: "takeover", args: "<run-id>", summary: "stop the phase and resume the agent interactively"},
	{name: "handback", args: "<run-id>", summary: "return a taken-over run to Mergeyard"},
	{name: "stop", args: "<run-id>", summary: "stop a run"},
	{name: "retry", args: "<run-id>", summary: "retry a failed or needs-attention run"},
	{name: "reconcile", summary: "report orphaned claims, worktrees, and sessions"},
}

// globalOptions holds flags accepted by every command.
type globalOptions struct {
	configPath string // empty means use the PRD §26 search order
	help       bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, args, err := parseGlobalFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard: %v\n", err)
		return 2
	}
	if opts.help || (len(args) > 0 && args[0] == "help") {
		printUsage(stdout)
		return 0
	}
	if len(args) == 0 {
		args = []string{"start"}
	}
	cmd, rest, err := lookup(args)
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard: %v\n\n", err)
		printUsage(stderr)
		return 2
	}
	if len(rest) != cmd.arity() {
		fmt.Fprintf(stderr, "usage: mergeyard %s\n", cmd.usage())
		return 2
	}
	if cmd.name == "start" {
		if opts.configPath != "" {
			err := &fault.Error{Code: "config.start_not_implemented", Path: opts.configPath, Err: errors.New("configuration startup integration is not implemented yet (see #14)")}
			fmt.Fprintf(stderr, "mergeyard start: %v\n", err)
			return 1
		}
		return start(stdout, stderr)
	}
	if cmd.name == "doctor" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return checkSetup(ctx, opts.configPath, stdout, stderr)
	}
	if cmd.name == "init" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		path, err := initialize(ctx, opts.configPath, os.Stdin, stdout)
		if err != nil {
			fmt.Fprintf(stderr, "mergeyard init: %v\n", err)
			return 1
		}
		if path == "" {
			return 0
		}
		return checkSetup(ctx, path, stdout, stderr)
	}
	if cmd.name == "repo add" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := addRepository(ctx, opts.configPath, rest[0], os.Stdin, stdout); err != nil {
			fmt.Fprintf(stderr, "mergeyard repo add: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stderr, "mergeyard %s: not implemented\n", cmd.name)
	return 1
}

func checkSetup(ctx context.Context, path string, stdout, stderr io.Writer) int {
	report := doctor.Check(ctx, path, doctor.Options{})
	if err := report.Write(stdout); err != nil {
		fmt.Fprintf(stderr, "mergeyard doctor: write report: %v\n", err)
		return 1
	}
	if report.HasErrors() {
		return 1
	}
	return 0
}

func start(stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := app.Open(ctx, "")
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard start: %v\n", err)
		return 1
	}
	defer runtime.Close()
	// Dispatch/startup orchestration belongs to #14. Connect the dashboard's
	// read model and stop control now, using the resolved config when available.
	cfg, doc, configErr := config.Load("")
	configPath := ""
	if configErr != nil {
		if !errors.Is(configErr, os.ErrNotExist) {
			fmt.Fprintf(stderr, "mergeyard start: %v\n", configErr)
			return 1
		}
		cfg, _, _ = config.Parse([]byte("{}"))
	} else {
		configPath = doc.Path
	}
	// Until #14 applies configured startup paths, report the actual process
	// workspace and listener rather than presenting unapplied values as effective.
	cfg.Workspace, cfg.Port = runtime.Workspace.Root, 7331
	engine, err := scheduler.New(cfg, scheduler.Resources{DB: runtime.DB, Events: runtime.Events, Workflow: runtime.Workflow, Workspace: runtime.Workspace, Control: runtime.Scheduler}, scheduler.Dependencies{})
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard start: %v\n", err)
		return 1
	}
	dashboard, err := web.NewDashboard(runtime.Events, runtime.Scheduler, web.DashboardOptions{
		DB: runtime.DB, Workspace: runtime.Workspace, Scheduler: engine, Config: cfg, ConfigPath: configPath,
		Diagnostics: func(checkCtx context.Context) doctor.Report {
			return doctor.Check(checkCtx, configPath, doctor.Options{})
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard start: %v\n", err)
		return 1
	}
	updatesCtx, cancelUpdates := context.WithCancel(ctx)
	updatesDone := make(chan struct{})
	go func() {
		defer close(updatesDone)
		dashboard.RunUpdates(updatesCtx)
	}()
	defer func() { cancelUpdates(); <-updatesDone }()
	listener, err := web.Listen("127.0.0.1:7331")
	if err != nil {
		fmt.Fprintf(stderr, "mergeyard start: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "mergeyard: workspace ready at %s; dashboard at http://%s (configuration startup integration and issue dispatch not implemented yet; see #14)\n", runtime.Workspace.Root, listener.Addr())
	serveErr := dashboard.Serve(ctx, listener)
	cancelUpdates()
	<-updatesDone
	if err := errors.Join(serveErr, runtime.Close()); err != nil {
		fmt.Fprintf(stderr, "mergeyard shutdown: %v\n", err)
		return 1
	}
	return 0
}

// parseGlobalFlags extracts global flags from anywhere in args, so
// "mergeyard --config x status" and "mergeyard status --config x" are
// equivalent. Arguments after "--" are positional.
func parseGlobalFlags(args []string) (globalOptions, []string, error) {
	var opts globalOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg[1:], "-"), "=")
		switch name {
		case "h", "help":
			if hasValue {
				return opts, nil, fmt.Errorf("flag does not take a value: %s", arg)
			}
			opts.help = true
		case "config":
			// A separate value starting with "-" is a missing value, not a path;
			// "--config=-x" remains possible.
			if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
			}
			if value == "" {
				return opts, nil, errors.New("flag needs an argument: --config")
			}
			opts.configPath = value
		default:
			return opts, nil, fmt.Errorf("unknown flag: %s", arg)
		}
	}
	return opts, positional, nil
}

// lookup finds the command whose name matches the leading words of args and
// returns it with the remaining arguments.
func lookup(args []string) (command, []string, error) {
	unknown := args[0]
	for _, c := range commands {
		words := strings.Fields(c.name)
		if len(args) >= len(words) && slices.Equal(args[:len(words)], words) {
			return c, args[len(words):], nil
		}
		// Name the subcommand too when args[0] is a group such as "repo".
		if len(words) > 1 && words[0] == args[0] && len(args) > 1 {
			unknown = args[0] + " " + args[1]
		}
	}
	return command{}, nil, fmt.Errorf("unknown command %q", unknown)
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: mergeyard [--config <path>] <command> [args]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  mergeyard %-24s %s\n", c.usage(), c.summary)
	}
	fmt.Fprint(w, "\nGlobal flags:\n"+
		"  --config <path>   config file (default: ./mergeyard.yaml, then ~/.config/mergeyard/config.yaml)\n"+
		"  -h, --help        show this help\n")
}
