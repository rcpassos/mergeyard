package sessions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWrapperRejectsEqualsInExecutable(t *testing.T) {
	dir := t.TempDir()
	_, err := wrapper(Command{Executable: filepath.Join(dir, "agent=alias"), Dir: dir}, dir)
	if err == nil {
		t.Fatal("env would interpret executable as an assignment; want a launch error")
	}
}

func TestWrapperDoesNotPublishFailedMetadataWrite(t *testing.T) {
	dir := t.TempDir()
	script, err := wrapper(Command{Executable: "/bin/sh", Args: []string{"-c", "exit 0"}, Dir: dir}, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a partial exit-metadata write after the process journal succeeds.
	script = strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\nprintf() { case \"$1\" in *exit_code*) /bin/echo '{'; return 1;; *) command printf \"$@\";; esac; }\n", 1)
	path := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/bin/sh", path).CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v: %s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "exit.json")); !os.IsNotExist(err) {
		t.Fatalf("failed write was published as metadata: %q, err=%v", data, err)
	}
}

func TestQuoteRoundTripsLiteralShellArguments(t *testing.T) {
	values := []string{"", "plain", "a b", "'\"; $HOME $(echo injected) `echo injected`", "line one\nline two", "-leading-dash", "界"}
	for _, value := range values {
		out, err := exec.Command("/bin/sh", "-c", "printf '%s' "+quote(value)).CombinedOutput()
		if err != nil {
			t.Fatalf("quote %q: %v: %s", value, err, out)
		}
		if string(out) != value {
			t.Fatalf("quote %q produced %q", value, out)
		}
	}
}

func TestWrapperValidationDoesNotRequireTmux(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cases := []Command{
		{Executable: "/bin/sh", Env: map[string]string{"": "value"}},
		{Executable: "/bin/sh", Env: map[string]string{"BAD=KEY": "value"}},
		{Executable: "/bin/sh", Env: map[string]string{"1BAD": "value"}},
		{Executable: "/bin/sh", Env: map[string]string{"VALID": "value\x00tail"}},
		{Executable: "/bin/sh", Args: []string{"arg\x00tail"}},
		{Executable: "/bin/sh", StdinPath: "file\x00tail"},
	}
	for _, command := range cases {
		_, err := wrapper(command, t.TempDir())
		assertCode(t, err, "phase.invalid_request")
	}
}

func TestGeneratedWrapperLogsAndMetadataWithoutTmux(t *testing.T) {
	dir := t.TempDir()
	phaseDir := filepath.Join(dir, "logs '$ ;")
	if err := os.Mkdir(phaseDir, 0700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input '$ ;")
	if err := os.WriteFile(input, []byte("input data\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value := "literal ' ; $(echo injected)\nsecond line"
	script, err := wrapper(Command{
		Executable: "/bin/sh", Args: []string{"-c", `printf '%s\n' "$VALUE" "$1"; /bin/cat; printf 'diagnostic' >&2; exit 7`, "fixture", value},
		Dir: dir, Env: map[string]string{"VALUE": value}, StdinPath: input,
	}, phaseDir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "wrapper.sh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/sh", path).Run(); err == nil {
		t.Fatal("wrapper discarded exit 7")
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("wrapper exit: %v", err)
		}
	}
	out, err := os.ReadFile(filepath.Join(phaseDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != value+"\n"+value+"\ninput data\n" {
		t.Fatalf("stdout = %q", out)
	}
	stderr, err := os.ReadFile(filepath.Join(phaseDir, "stderr.log"))
	if err != nil || string(stderr) != "diagnostic" {
		t.Fatalf("stderr = %q, err=%v", stderr, err)
	}
	status, err := New(Options{}).Status(context.Background(), Ref{Name: "not-needed", PhaseDir: phaseDir})
	if err != nil || status.State != Exited || status.ExitCode == nil || *status.ExitCode != 7 {
		t.Fatalf("metadata = %+v, err=%v", status, err)
	}
}
