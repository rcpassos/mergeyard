package escalation_test

import (
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/escalation"
)

func TestRulesFromDiff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []escalation.FileChange
		want    []escalation.Flag
	}{
		{"ci and deploy sorted and grouped", []escalation.FileChange{{Path: "infra/main.tf"}, {Path: "Dockerfile.prod"}, {Path: ".github/workflows/ci.yml"}, {Path: "api/Dockerfile"}}, []escalation.Flag{{Reason: "ci/deploy", Files: []string{".github/workflows/ci.yml", "Dockerfile.prod", "api/Dockerfile", "infra/main.tf"}}}},
		{"root and nested migrations", []escalation.FileChange{{Path: "migrations/001.sql"}, {Path: "db/migrations/002.sql"}}, []escalation.Flag{{Reason: "migration", Files: []string{"db/migrations/002.sql", "migrations/001.sql"}}}},
		{"renames out of sensitive paths", []escalation.FileChange{{OldPath: ".github/workflows/ci.yml", Path: "ci.yml"}, {OldPath: "db/migrations/001.sql", Path: "db/001.sql"}}, []escalation.Flag{{Reason: "ci/deploy", Files: []string{".github/workflows/ci.yml"}}, {Reason: "migration", Files: []string{"db/migrations/001.sql"}}}},
		{"all manifests including nested", []escalation.FileChange{{Path: "go.mod"}, {Path: "ui/package.json"}, {Path: "pyproject.toml"}, {Path: "Cargo.toml"}, {Path: "Gemfile"}}, []escalation.Flag{{Reason: "dependency manifests", Files: []string{"Cargo.toml", "Gemfile", "go.mod", "pyproject.toml", "ui/package.json"}}}},
		{"lockfiles alone", []escalation.FileChange{{Path: "go.sum"}, {Path: "ui/package-lock.json"}, {Path: "Cargo.lock"}, {Path: "Gemfile.lock"}, {Path: "uv.lock"}}, []escalation.Flag{}},
		{"at size boundary", []escalation.FileChange{{Path: "small.go", Additions: 200, Deletions: 200}}, []escalation.Flag{}},
		{"additions and deletions above size boundary", []escalation.FileChange{{Path: "b.go", Additions: 200}, {Path: "a.go", Deletions: 201}}, []escalation.Flag{{Reason: "size: 401 changed lines exceeds 400", Files: []string{"a.go", "b.go"}}}},
		{"no auth defaults or substring matches", []escalation.FileChange{{Path: "auth/login.go"}, {Path: "money/pay.go"}, {Path: "old-migrations/001.sql"}, {Path: "Dockerfiles/readme.md"}}, []escalation.Flag{}},
		{"empty diff", nil, []escalation.Flag{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for range 10 {
				if got := escalation.Evaluate(escalation.Defaults(), tc.changes); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %+v, want %+v", got, tc.want)
				}
			}
		})
	}
}

func TestCustomRulesOverlapAndDisabledRules(t *testing.T) {
	rules := escalation.Rules{Paths: map[string]string{"**/auth/**": "auth", "services/**": "auth", "*.go": "root code", "**/**/pay?.[gt]o": "money"}}
	changes := []escalation.FileChange{{Path: "services/auth/login.go", Additions: 1000}, {Path: "auth/login.go"}, {Path: "main.go"}, {Path: "nested/main.go"}, {Path: "pay1.go"}, {Path: "services/pay2.to"}, {Path: "package.json"}}
	want := []escalation.Flag{
		{Reason: "auth", Files: []string{"auth/login.go", "services/auth/login.go", "services/pay2.to"}},
		{Reason: "money", Files: []string{"pay1.go", "services/pay2.to"}},
		{Reason: "root code", Files: []string{"main.go", "pay1.go"}},
	}
	if got := escalation.Evaluate(rules, changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
