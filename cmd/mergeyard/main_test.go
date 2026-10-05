package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionWithoutConfiguration(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"--config", "/nonexistent/config.yaml", "version"}} {
		code, stdout, stderr := runCLI(args...)
		if code != 0 || stdout != "mergeyard "+version+"\n" || stderr != "" {
			t.Fatalf("version: code %d stdout %q stderr %q", code, stdout, stderr)
		}
	}
}

func TestStartRequiresConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, args := range [][]string{{"start", "--config", path}, {"--config", path}} {
		code, stdout, stderr := runCLI(args...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "config.not_found") {
			t.Fatalf("startup: code %d stdout %q stderr %q", code, stdout, stderr)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing config was changed: %v", err)
	}
}

// PRD §29.
var prdCommands = []string{
	"init",
	"repo add <owner/repo>",
	"status",
	"doctor",
	"open",
	"pause",
	"resume",
	"watch <run-id>",
	"takeover <run-id>",
	"handback <run-id>",
	"stop <run-id>",
	"retry <run-id>",
	"reconcile",
	"start",
}

func TestHelpListsEveryCommand(t *testing.T) {
	for _, flag := range []string{"--help", "-h", "help"} {
		t.Run(flag, func(t *testing.T) {
			code, stdout, _ := runCLI(flag)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			for _, cmd := range prdCommands {
				if !strings.Contains(stdout, "mergeyard "+cmd) {
					t.Errorf("help does not list %q:\n%s", cmd, stdout)
				}
			}
			if !strings.Contains(stdout, "--config <path>") {
				t.Errorf("help does not document --config:\n%s", stdout)
			}
		})
	}
}

func TestUnimplementedCommandsFail(t *testing.T) {
	invocations := [][]string{
		{"open"},
		{"takeover", "run-1"},
		{"handback", "run-1"},
		{"retry", "run-1"},
	}
	for _, args := range invocations {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(args...)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr, "not implemented") {
				t.Errorf("stderr = %q, want it to contain %q", stderr, "not implemented")
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"frobnicate"}, `unknown command "frobnicate"`},
		{[]string{"repo"}, `unknown command "repo"`},
		{[]string{"repo", "remove", "octo/repo"}, `unknown command "repo remove"`},
		{[]string{"repo", "add"}, "usage: mergeyard repo add <owner/repo>"},
		{[]string{"watch"}, "usage: mergeyard watch <run-id>"},
		{[]string{"stop", "run-1", "run-2"}, "usage: mergeyard stop <run-id>"},
		{[]string{"status", "extra"}, "usage: mergeyard status"},
		{[]string{"version", "extra"}, "usage: mergeyard version"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, stdout, stderr := runCLI(tc.args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestConfigFlag(t *testing.T) {
	accepted := [][]string{
		{"--config", "my.yaml"},
		{"--config", "my.yaml", "status"},
		{"--config=my.yaml", "status"},
		{"-config", "my.yaml", "status"},
		{"status", "--config", "my.yaml"},
		{"watch", "run-1", "--config", "my.yaml"},
	}
	for _, args := range accepted {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, _, stderr := runCLI(args...)
			if code != 1 || !strings.Contains(stderr, "config.not_found") {
				t.Errorf("got exit %d, stderr %q; want dispatch to reach the command", code, stderr)
			}
		})
	}

	rejected := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"status", "--config"}, "flag needs an argument: --config"},
		{[]string{"--config="}, "flag needs an argument: --config"},
		{[]string{"--config", "--help"}, "flag needs an argument: --config"},
		{[]string{"status", "--config", "--", "x"}, "flag needs an argument: --config"},
		{[]string{"---config=x", "status"}, "unknown flag: ---config=x"},
		{[]string{"--help=false", "status"}, "flag does not take a value: --help=false"},
		{[]string{"--version=false"}, "flag does not take a value: --version=false"},
		{[]string{"--verbose", "status"}, "unknown flag: --verbose"},
		{[]string{"status", "-x"}, "unknown flag: -x"},
	}
	for _, tc := range rejected {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			code, _, stderr := runCLI(tc.args...)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tc.wantErr)
			}
		})
	}
}
