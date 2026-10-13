package config_test

import (
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/escalation"
)

func TestEscalationDefaultsAndRepositoryOverrides(t *testing.T) {
	cfg, doc, err := config.Parse([]byte(`repositories:
  - repo: owner/defaults
  - repo: owner/custom
    escalation:
      max_changed_lines: 100
      paths: {"**/auth/**": auth}
      new_dependencies: false
  - repo: owner/disabled
    escalation:
      max_changed_lines: 0
      paths: {}
      new_dependencies: false
`))
	if err != nil || len(doc.Warnings) != 0 {
		t.Fatalf("parse: %v %+v", err, doc)
	}
	if !reflect.DeepEqual(cfg.Escalation, escalation.Defaults()) || !reflect.DeepEqual(cfg.Repositories[0].Escalation, escalation.Defaults()) {
		t.Fatal("missing defaults")
	}
	custom := cfg.Repositories[1].Escalation
	if custom.MaxChangedLines != 100 || custom.NewDependencies || !reflect.DeepEqual(custom.Paths, map[string]string{"**/auth/**": "auth"}) {
		t.Fatalf("override: %+v", custom)
	}
	if got := escalation.Evaluate(cfg.Repositories[2].Escalation, []escalation.FileChange{{Path: ".github/ci.yml", Additions: 1000}, {Path: "package.json"}}); len(got) != 0 {
		t.Fatalf("disabled: %+v", got)
	}
	cfg.Repositories[0].Escalation.Paths[".github/**"] = "changed"
	if cfg.Escalation.Paths[".github/**"] != "ci/deploy" {
		t.Fatal("repository mutates global paths")
	}
	cfg, _, err = config.Parse([]byte(`escalation:
  max_changed_lines: 25
  paths: {"**/payments/**": money}
  new_dependencies: false
repositories:
  - repo: owner/inherits
  - repo: owner/partial
    escalation: {max_changed_lines: 50}
`))
	if err != nil || cfg.Repositories[0].Escalation.MaxChangedLines != 25 || cfg.Repositories[1].Escalation.MaxChangedLines != 50 || cfg.Repositories[1].Escalation.NewDependencies || !reflect.DeepEqual(cfg.Repositories[1].Escalation.Paths, cfg.Escalation.Paths) {
		t.Fatalf("global inheritance: %+v %v", cfg, err)
	}
}

func TestEscalationRejectsInvalidRules(t *testing.T) {
	for _, rule := range []string{
		`max_changed_lines: -1`, `max_changed_lines: 1.5`, `max_changed_lines: "400"`, `new_dependencies: []`,
		`paths: null`, `paths: []`, `paths: {"[": ci}`, `paths: {"": ci}`, `paths: {"/etc/**": ci}`,
		`paths: {"../foo": ci}`, `paths: {"a//b": ci}`, `paths: {foo: ""}`, `paths: {foo: 12}`, `paths: {foo: "a\nb"}`, `paths: {foo: ci, foo: migration}`,
	} {
		t.Run(rule, func(t *testing.T) {
			for _, prefix := range []string{"escalation:\n  ", "repositories:\n  - repo: owner/repo\n    enabled: false\n    escalation:\n      "} {
				if _, _, err := config.Parse([]byte(prefix + rule + "\n")); err == nil {
					t.Fatalf("accepted %s", rule)
				}
			}
		})
	}
}
