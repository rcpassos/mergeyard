package git_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rcpassos/mergeyard/internal/escalation"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

func TestEscalationDiffUsesPinnedHeadAndExactPaths(t *testing.T) {
	f := newFixture(t)
	run := prepare(t, f)
	for name, content := range map[string]string{"package.json": "{}\n", "odd\tname\n.go": "one\ntwo\n", "binary.tf": "\x00binary"} {
		write(t, run.Path, name, content)
	}
	write(t, run.Path, "README.md", "changed\n")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "approved")
	head := git(t, run.Path, "rev-parse", "HEAD")
	write(t, run.Path, "later.go", "not approved\n")
	git(t, run.Path, "add", ".")
	git(t, run.Path, "commit", "-m", "later")
	laterHead := git(t, run.Path, "rev-parse", "HEAD")
	got, err := f.manager.EscalationDiff(context.Background(), run, head)
	want := []escalation.FileChange{{Path: "README.md", Additions: 1, Deletions: 1}, {Path: "binary.tf"}, {Path: "odd\tname\n.go", Additions: 2}, {Path: "package.json", Additions: 1}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("diff: %+v, %v; want %+v", got, err, want)
	}
	if _, err := f.manager.EscalationDiff(context.Background(), run, "--bad-target"); err == nil {
		t.Fatal("invalid target accepted")
	}
	// Paths at both ends of a rename participate, even for identical contents.
	if err := os.MkdirAll(filepath.Join(run.Path, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, run.Path, "mv", "package.json", "migrations/schema.sql")
	git(t, run.Path, "commit", "-m", "rename")
	renameRun := run
	renameRun.BaseSHA = laterHead
	got, err = f.manager.EscalationDiff(context.Background(), renameRun, git(t, run.Path, "rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "migrations/schema.sql" || got[0].OldPath != "package.json" || got[0].Additions != 0 || got[0].Deletions != 0 {
		t.Fatalf("rename inflated size or lost paths: %+v", got)
	}
	flags := escalation.Evaluate(escalation.Defaults(), got)
	if len(flags) != 2 || flags[0].Reason != "dependency manifests" || flags[1].Reason != "migration" {
		t.Fatalf("rename missed sensitive paths: %+v", flags)
	}
	// Ownership checks still apply to this read-only adapter.
	wrong := managedgit.Run{}
	if _, err := f.manager.EscalationDiff(context.Background(), wrong, head); err == nil {
		t.Fatal("unowned run accepted")
	}
}

func TestEscalationDiffPinsLineCountingSettings(t *testing.T) {
	for _, scope := range []string{"global", "repository"} {
		t.Run(scope, func(t *testing.T) {
			f := newFixture(t)
			text := func(chars string, extras int) string {
				var out strings.Builder
				for i := 0; i < 30; i++ {
					fmt.Fprintf(&out, "anchor %d\n", i)
					for _, char := range chars {
						fmt.Fprintf(&out, "%d %c\n", i, char)
					}
				}
				for i := 0; i < extras; i++ {
					fmt.Fprintf(&out, "extra %d\n", i)
				}
				return out.String()
			}
			write(t, f.seed, "example.txt", text("EECADBCBAEBBCEBBDEDBACAC", 0))
			git(t, f.seed, "add", ".")
			git(t, f.seed, "commit", "-m", "base text")
			git(t, f.seed, "push", "origin", "main")
			run := prepare(t, f)
			write(t, run.Path, "example.txt", text("EDCEDECAAEBBCABBDEDCACAC", 39))
			git(t, run.Path, "add", ".")
			git(t, run.Path, "commit", "-m", "approved text")
			head := git(t, run.Path, "rev-parse", "HEAD")
			configArgs := []string{"config"}
			if scope == "global" {
				file := filepath.Join(t.TempDir(), "gitconfig")
				t.Setenv("GIT_CONFIG_GLOBAL", file)
				configArgs = append(configArgs, "--file", file)
			}
			for _, algorithm := range []string{"myers", "histogram", "patience", "minimal"} {
				t.Run(algorithm, func(t *testing.T) {
					git(t, run.Path, append(append([]string{}, configArgs...), "diff.algorithm", algorithm)...)
					git(t, run.Path, append(append([]string{}, configArgs...), "diff.indentHeuristic", "true")...)
					got, err := f.manager.EscalationDiff(context.Background(), run, head)
					want := []escalation.FileChange{{Path: "example.txt", Additions: 219, Deletions: 180}}
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("configured %s: diff=%+v, err=%v; want %+v", algorithm, got, err, want)
					}
					if flags := escalation.Evaluate(escalation.Defaults(), got); len(flags) != 0 {
						t.Fatalf("399 changed lines acquired a size flag: %+v", flags)
					}
				})
			}
		})
	}
}

func TestEscalationDiffIncludesIgnoredGitlinks(t *testing.T) {
	f := newFixture(t)
	child := filepath.Join(t.TempDir(), "child")
	git(t, f.seed, "init", "--initial-branch=main", child)
	write(t, child, "action.txt", "first\n")
	git(t, child, "add", ".")
	git(t, child, "commit", "-m", "first child commit")
	first := git(t, child, "rev-parse", "HEAD")
	write(t, child, "action.txt", "second\n")
	git(t, child, "commit", "-am", "second child commit")
	second := git(t, child, "rev-parse", "HEAD")
	write(t, f.seed, ".gitmodules", fmt.Sprintf("[submodule \"action\"]\n path = .github/action\n url = %s\n ignore = all\n", child))
	git(t, f.seed, "add", ".gitmodules")
	git(t, f.seed, "update-index", "--add", "--cacheinfo", "160000,"+first+",.github/action")
	git(t, f.seed, "commit", "-m", "base gitlink")
	git(t, f.seed, "push", "origin", "main")
	run := prepare(t, f)
	git(t, run.Path, "update-index", "--cacheinfo", "160000,"+second+",.github/action")
	git(t, run.Path, "commit", "-m", "approved gitlink")
	head := git(t, run.Path, "rev-parse", "HEAD")
	for _, policy := range []string{"all", "dirty", "untracked", "none"} {
		t.Run(policy, func(t *testing.T) {
			git(t, run.Path, "config", "diff.ignoreSubmodules", policy)
			git(t, run.Path, "config", "submodule.action.ignore", "all")
			got, err := f.manager.EscalationDiff(context.Background(), run, head)
			want := []escalation.FileChange{{Path: ".github/action", Additions: 1, Deletions: 1}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("ignore %s: diff=%+v, err=%v; want %+v", policy, got, err, want)
			}
			wantFlags := []escalation.Flag{{Reason: "ci/deploy", Files: []string{".github/action"}}}
			if flags := escalation.Evaluate(escalation.Defaults(), got); !reflect.DeepEqual(flags, wantFlags) {
				t.Fatalf("gitlink CI flag: %+v, want %+v", flags, wantFlags)
			}
		})
	}
}

func TestEscalationDiffIgnoresUserAttributes(t *testing.T) {
	for _, source := range []string{"global config", "repository config", "default XDG file"} {
		t.Run(source, func(t *testing.T) {
			f := newFixture(t)
			write(t, f.seed, ".gitattributes", "opaque.go -diff\n")
			write(t, f.seed, "feature.go", "base\n")
			git(t, f.seed, "add", ".")
			git(t, f.seed, "commit", "-m", "base attributes and text")
			git(t, f.seed, "push", "origin", "main")
			run := prepare(t, f)
			var content strings.Builder
			for i := 0; i < 401; i++ {
				fmt.Fprintf(&content, "line %d\n", i)
			}
			write(t, run.Path, "feature.go", content.String())
			write(t, run.Path, "opaque.go", "repository marks this text binary\n")
			write(t, run.Path, "binary.tf", "\x00binary\n")
			git(t, run.Path, "add", ".")
			git(t, run.Path, "commit", "-m", "approved text and binary")
			head := git(t, run.Path, "rev-parse", "HEAD")
			want := []escalation.FileChange{{Path: "binary.tf"}, {Path: "feature.go", Additions: 401, Deletions: 1}, {Path: "opaque.go"}}
			wantFlags := escalation.Evaluate(escalation.Defaults(), want)
			if len(wantFlags) != 2 || wantFlags[1].Reason != "size: 402 changed lines exceeds 400" {
				t.Fatalf("invalid size-flag fixture: %+v", wantFlags)
			}
			assertDiff := func() {
				t.Helper()
				got, err := f.manager.EscalationDiff(context.Background(), run, head)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("diff=%+v, err=%v; want %+v", got, err, want)
				}
				if flags := escalation.Evaluate(escalation.Defaults(), got); !reflect.DeepEqual(flags, wantFlags) {
					t.Fatalf("flags=%+v; want %+v", flags, wantFlags)
				}
			}
			assertDiff()
			attributesRoot := t.TempDir()
			write(t, attributesRoot, "attributes", "*.go -diff\n*.tf diff\n")
			attributes := filepath.Join(attributesRoot, "attributes")
			switch source {
			case "global config":
				config := filepath.Join(t.TempDir(), "gitconfig")
				t.Setenv("GIT_CONFIG_GLOBAL", config)
				git(t, run.Path, "config", "--file", config, "core.attributesFile", attributes)
			case "repository config":
				git(t, run.Path, "config", "core.attributesFile", attributes)
			case "default XDG file":
				// Git's implicit user attributes live at $XDG_CONFIG_HOME/git/attributes.
				if err := os.MkdirAll(filepath.Join(attributesRoot, "git"), 0700); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(attributesRoot, "git"), "attributes", "*.go -diff\n*.tf diff\n")
				t.Setenv("XDG_CONFIG_HOME", attributesRoot)
			}
			// Confirm each source really changes ordinary Git's binary policy.
			if raw := git(t, run.Path, "diff", "--numstat", run.BaseSHA+"..."+head); !strings.Contains(raw, "-\t-\tfeature.go") || !strings.Contains(raw, "1\t0\tbinary.tf") {
				t.Fatalf("attributes fixture did not affect ordinary diff: %q", raw)
			}
			assertDiff()
		})
	}
}

func TestEscalationDiffPinsBinarySizeCutoff(t *testing.T) {
	for _, scope := range []string{"global", "repository"} {
		t.Run(scope, func(t *testing.T) {
			f := newFixture(t)
			write(t, f.seed, "feature.go", "base\n")
			git(t, f.seed, "add", ".")
			git(t, f.seed, "commit", "-m", "base text")
			git(t, f.seed, "push", "origin", "main")
			run := prepare(t, f)
			var content strings.Builder
			for i := 0; i < 401; i++ {
				fmt.Fprintf(&content, "line %d\n", i)
			}
			write(t, run.Path, "feature.go", content.String())
			git(t, run.Path, "add", ".")
			git(t, run.Path, "commit", "-m", "approved text")
			head := git(t, run.Path, "rev-parse", "HEAD")
			configArgs := []string{"config"}
			if scope == "global" {
				file := filepath.Join(t.TempDir(), "gitconfig")
				t.Setenv("GIT_CONFIG_GLOBAL", file)
				configArgs = append(configArgs, "--file", file)
			}
			want := []escalation.FileChange{{Path: "feature.go", Additions: 401, Deletions: 1}}
			wantFlags := []escalation.Flag{{Reason: "size: 402 changed lines exceeds 400", Files: []string{"feature.go"}}}
			for _, threshold := range []string{"512m", "1"} {
				git(t, run.Path, append(append([]string{}, configArgs...), "core.bigFileThreshold", threshold)...)
				got, err := f.manager.EscalationDiff(context.Background(), run, head)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("cutoff %s: diff=%+v, err=%v; want %+v", threshold, got, err, want)
				}
				if flags := escalation.Evaluate(escalation.Defaults(), got); !reflect.DeepEqual(flags, wantFlags) {
					t.Fatalf("cutoff %s: flags=%+v; want %+v", threshold, flags, wantFlags)
				}
			}
		})
	}
}
