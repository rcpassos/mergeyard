// Command mergeyard turns ready GitHub issues into reviewed pull requests.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// command is one entry in the PRD §29 command table.
type command struct {
	name    string
	args    string // usage placeholder, e.g. "<run-id>"
	summary string
	// run executes the command; nil means it is not implemented yet.
	run func(opts globalOptions, args []string) error
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// globalOptions holds flags accepted by every command.
type globalOptions struct {
	configPath string // empty means use the PRD §26 search order
	help       bool
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
	cmd, rest, ok := lookup(args)
	if !ok {
		fmt.Fprintf(stderr, "mergeyard: unknown command %q\n\n", unknownName(args))
		printUsage(stderr)
		return 2
	}
	if len(rest) != len(strings.Fields(cmd.args)) {
		fmt.Fprintf(stderr, "usage: mergeyard %s\n", cmd.usage())
		return 2
	}
	if cmd.run == nil {
		fmt.Fprintf(stderr, "mergeyard %s: not implemented\n", cmd.name)
		return 1
	}
	if err := cmd.run(opts, rest); err != nil {
		fmt.Fprintf(stderr, "mergeyard %s: %v\n", cmd.name, err)
		return 1
	}
	return 0
}

func (c command) usage() string {
	if c.args == "" {
		return c.name
	}
	return c.name + " " + c.args
}

// unknownName names an unmatched command for error messages, keeping the
// subcommand word when args[0] is a command group such as "repo".
func unknownName(args []string) string {
	for _, c := range commands {
		words := strings.Fields(c.name)
		if len(words) > 1 && words[0] == args[0] && len(args) > 1 {
			return args[0] + " " + args[1]
		}
	}
	return args[0]
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
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "h", "help":
			opts.help = true
		case "config":
			if !hasValue {
				if i+1 < len(args) {
					i++
					value = args[i]
				}
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
func lookup(args []string) (command, []string, bool) {
	for _, c := range commands {
		words := strings.Fields(c.name)
		if len(args) >= len(words) && slices.Equal(args[:len(words)], words) {
			return c, args[len(words):], true
		}
	}
	return command{}, nil, false
}

func printUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString("Usage: mergeyard [--config <path>] <command> [args]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  mergeyard %-24s %s\n", c.usage(), c.summary)
	}
	b.WriteString("\nGlobal flags:\n  --config <path>   config file (default: ./mergeyard.yaml, then ~/.config/mergeyard/config.yaml)\n")
	io.WriteString(w, b.String())
}
