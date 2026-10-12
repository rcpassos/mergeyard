package git_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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
