// Command mergeyard turns ready GitHub issues into draft pull requests.
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

	"github.com/rcpassos/mergeyard/internal/doctor"
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
	{name: "version", summary: "show the installed version"},
	{name: "start", summary: "start the scheduler and dashboard (default)"},
	{name: "init", summary: "interactive setup: writes config, checks tools"},
	{name: "try", summary: "create a private playground and try the full loop"},
	{name: "repo add", args: "<owner/repo>", summary: "add a repository to the config"},
	{name: "plan", summary: "preview claims and explain the ready queue (read-only)"},
	{name: "status", summary: "show scheduler and run status"},
	{name: "harness check", args: "<claude|codex>", summary: "use account quota for a minimal credit recovery check"},
	{name: "doctor", summary: "check tools, auth, and config"},
	{name: "open", summary: "open the dashboard in a browser"},
	{name: "pause", summary: "stop claiming new issues"},
	{name: "resume", summary: "resume claiming new issues"},
	{name: "watch", args: "<run-id>", summary: "attach read-only to a run's session"},
	{name: "takeover", args: "<run-id>", summary: "stop the phase and resume the agent interactively"},
	{name: "handback", args: "<run-id>", summary: "return a taken-over run to Mergeyard"},
	{name: "stop", args: "<run-id>", summary: "stop a run"},
	{name: "retry", args: "<run-id>", summary: "retry a failed or needs-attention run"},
	{name: "requeue", args: "<run-id>", summary: "requeue a run's stopped publications"},
	{name: "reconcile", summary: "report orphaned claims, worktrees, and sessions"},
}

// globalOptions holds shared flags and the Retry-only guidance options.
type globalOptions struct {
	planRepo      *string
	planJSON      bool
	configPath    string // empty means use the PRD §26 search order
	help          bool
	version       bool
	retryNote     *string
	retryNoteFile *string
}

// Set by GoReleaser for release builds.
var version = "dev"

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
	if opts.version {
		fmt.Fprintf(stdout, "mergeyard %s\n", version)
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
	if (opts.retryNote != nil || opts.retryNoteFile != nil) && cmd.name != "retry" {
		fmt.Fprintln(stderr, "mergeyard: --note and --note-file require retry")
		return 2
	}
	if (opts.planRepo != nil || opts.planJSON) && cmd.name != "plan" {
		fmt.Fprintln(stderr, "mergeyard: --repo and --json require plan")
		return 2
	}
	if len(rest) != cmd.arity() {
		fmt.Fprintf(stderr, "usage: mergeyard %s\n", cmd.usage())
		return 2
	}
	if cmd.name == "retry" {
		if opts.retryNoteFile != nil {
			data, err := os.ReadFile(*opts.retryNoteFile)
			if err != nil {
				fmt.Fprintf(stderr, "mergeyard retry: read note file: %v\n", err)
				return 1
			}
			note := string(data)
			opts.retryNote = &note
		}
		if opts.retryNote != nil {
			rest = append(rest, *opts.retryNote)
		}
	}
	if cmd.name == "version" {
		fmt.Fprintf(stdout, "mergeyard %s\n", version)
		return 0
	}
	if cmd.name == "try" {
		if opts.configPath != "" {
			fmt.Fprintln(stderr, "mergeyard try: --config is not supported; the playground uses its own config")
			return 2
		}
		return tryPlayground(stdout, stderr)
	}
	if cmd.name == "start" {
		return start(opts.configPath, stdout, stderr)
	}
	if cmd.name == "doctor" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return checkSetup(ctx, opts.configPath, stdout, stderr)
	}
	if cmd.name == "plan" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		repository := ""
		if opts.planRepo != nil {
			repository = *opts.planRepo
		}
		if err := plan(ctx, opts.configPath, repository, opts.planJSON, stdout); err != nil {
			fmt.Fprintf(stderr, "mergeyard plan: %v\n", err)
			return 1
		}
		return 0
	}
	if cmd.name == "reconcile" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := reconcile(ctx, opts.configPath, stdout); err != nil {
			fmt.Fprintf(stderr, "mergeyard reconcile: %v\n", err)
			return 1
		}
		return 0
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
	if cmd.name == "harness check" || cmd.name == "status" || cmd.name == "pause" || cmd.name == "resume" || cmd.name == "open" || cmd.name == "watch" || cmd.name == "stop" || cmd.name == "retry" || cmd.name == "requeue" || cmd.name == "takeover" || cmd.name == "handback" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := operate(ctx, opts.configPath, cmd.name, rest, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "mergeyard %s: %v\n", cmd.name, err)
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
		case "json":
			if hasValue {
				return opts, nil, fmt.Errorf("flag does not take a value: %s", arg)
			}
			opts.planJSON = true
		case "repo":
			if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				value = args[i]
			}
			if value == "" {
				return opts, nil, errors.New("flag needs an argument: --repo")
			}
			opts.planRepo = &value
		case "version":
			if hasValue {
				return opts, nil, fmt.Errorf("flag does not take a value: %s", arg)
			}
			opts.version = true
		case "h", "help":
			if hasValue {
				return opts, nil, fmt.Errorf("flag does not take a value: %s", arg)
			}
			opts.help = true
		case "note", "note-file":
			if opts.retryNote != nil || opts.retryNoteFile != nil {
				return opts, nil, errors.New("use exactly one of --note or --note-file")
			}
			if !hasValue {
				if i+1 >= len(args) {
					return opts, nil, fmt.Errorf("flag needs an argument: --%s", name)
				}
				i++
				value = args[i]
			}
			if name == "note" {
				opts.retryNote = &value
			} else {
				if value == "" {
					return opts, nil, errors.New("flag needs an argument: --note-file")
				}
				opts.retryNoteFile = &value
			}
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
	fmt.Fprint(w, "\nRetry flags (optional, mutually exclusive):\n"+
		"  --note <text>       human guidance for the rest of the run\n"+
		"  --note-file <path>  read human guidance from a file\n"+
		"\nPlan flags:\n"+
		"  --repo <owner/repo>  show one configured repository\n"+
		"  --json              print stable JSON\n"+
		"\nGlobal flags:\n"+
		"  --config <path>   config file (default: ./mergeyard.yaml, then ~/.config/mergeyard/config.yaml)\n"+
		"  --version        show the installed version\n"+
		"  -h, --help        show this help\n")
}
