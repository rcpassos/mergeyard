package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportsConfigurationErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.yaml")
	code, out, errOut := runCLI("doctor", "--config", path)
	if code != 1 || errOut != "" || !strings.Contains(out, "error:\n") || !strings.Contains(out, "config.not_found") {
		t.Fatalf("doctor result: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestDoctorExitStatusAndGroups(t *testing.T) {
	for _, tc := range []struct {
		name, version, extra, finding string
		exit                          int
	}{
		{"healthy", "2.1.277 (Claude Code)", "", "error:\n  none\nwarning:\n  none\nunverifiable:\n  none\n", 0},
		{"unsupported version", "2.1.276 (Claude Code)", "", "harness.version_unsupported", 1},
		{"warning only", "2.1.277 (Claude Code)", "future_setting: true\n", "config.unknown_field", 0},
		{"unverifiable only", "development build", "", "harness.version_unverifiable", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, tools := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", tools)
			for name, script := range map[string]string{
				"git": "#!/bin/sh\nexit 0\n", "tmux": "#!/bin/sh\nexit 0\n", "gh": "#!/bin/sh\nexit 0\n",
				"claude": "#!/bin/sh\ncase \"$1\" in\n --version) echo '" + tc.version + "';;\n auth) exit 0;;\n *) exit 1;;\nesac\n",
			} {
				if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			listener.Close()
			path := filepath.Join(home, "config.yaml")
			data := fmt.Sprintf("port: %d\nworkspace: %q\nagents: {claude: {permission_mode: acceptEdits}}\n", port, filepath.Join(home, "workspace")) + tc.extra
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := runCLI("--config", path, "doctor")
			if code != tc.exit || errOut != "" || !strings.Contains(out, tc.finding) {
				t.Fatalf("doctor result: exit=%d stdout=%q stderr=%q", code, out, errOut)
			}
			if !strings.Contains(out, "warning:\n") || !strings.Contains(out, "unverifiable:\n") {
				t.Fatalf("missing groups: %s", out)
			}
			if _, err := os.Stat(filepath.Join(home, "workspace")); !os.IsNotExist(err) {
				t.Fatalf("doctor initialized workspace: %v", err)
			}
		})
	}
}
