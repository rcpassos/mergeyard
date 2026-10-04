package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
)

func TestWriterUpdatesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "config.yaml")
	writeFixture(t, target, "# managed config\nport: 7331\n")
	if err := os.Chmod(target, 0640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join("dotfiles", "config.yaml"), link); err != nil {
		t.Fatal(err)
	}
	_, doc, err := config.Load(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"port"}, 7444); err != nil {
		t.Fatal(err)
	}
	if err := doc.Write(link); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("writer replaced the config symlink")
	}
	cfg, _, err := config.Load(target)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 7444 {
		t.Errorf("symlink target port = %d; want 7444", cfg.Port)
	}
	info, err = os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("target mode = %v; want 0640", info.Mode())
	}
	if doc.Path != link {
		t.Errorf("source path = %q; want symlink %q", doc.Path, link)
	}
}

func TestAppendRepositoryCreatesMissingSequence(t *testing.T) {
	_, doc, err := config.Parse([]byte("# config without repositories\nport: 7331\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"repositories", "-"}, map[string]string{"repo": "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].Repo != "owner/repo" {
		t.Errorf("repositories = %#v", cfg.Repositories)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "# config without repositories") {
		t.Errorf("lost comment: %s", data)
	}
}

func TestNewConfigWritesBlockStyle(t *testing.T) {
	_, doc, err := config.Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"implementer", "agent"}, "codex"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Set([]string{"repositories", "-"}, map[string]string{"repo": "owner/repo"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "implementer:\n  agent: codex\nrepositories:\n  - repo: owner/repo\n"
	if string(data) != want {
		t.Errorf("new config = %q; want %q", data, want)
	}
}

func TestUnknownFieldsReturnWarningsAndSurviveWrites(t *testing.T) {
	data := `portt: 7444
agents:
  claude: {permision_mode: acceptEdits}
  codex: {sandbbox: danger-full-access}
  other: {executable: other}
implementer: {max_atempts: 2}
labels: {runnning: running}
usage_limits: {max_wait: 4}
repositories:
  - repo: owner/repo
    base_branc: develop
    reviewer: {skils: [review]}
`
	cfg, doc, err := config.Parse([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Claude.PermissionMode != "bypassPermissions" || cfg.Agents.Codex.Sandbox != "workspace-write" {
		t.Errorf("unknown keys changed defaults: %#v", cfg.Agents)
	}
	paths := []string{}
	for _, warning := range doc.Warnings {
		if warning.Code != "config.unknown_field" || warning.Message == "" {
			t.Errorf("warning = %#v", warning)
		}
		paths = append(paths, warning.Path)
	}
	want := []string{"labels.runnning", "agents.claude.permision_mode", "agents.codex.sandbbox", "agents.other", "implementer.max_atempts", "usage_limits.max_wait", "repositories[0].reviewer.skils", "repositories[0].base_branc", "portt"}
	slices.Sort(paths)
	slices.Sort(want)
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("warning paths = %v; want %v", paths, want)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := doc.Write(path); err != nil {
		t.Fatal(err)
	}
	_, loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Warnings, doc.Warnings) {
		t.Errorf("warnings changed after round trip: %#v", loaded.Warnings)
	}
	output, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "permision_mode: acceptEdits") {
		t.Errorf("writer lost unknown permission field: %s", output)
	}
}

func TestKnownFieldsProduceNoWarnings(t *testing.T) {
	_, doc, err := config.Load("../../mergeyard.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) != 0 {
		t.Errorf("known fields produced warnings: %#v", doc.Warnings)
	}
}
