package config_test

import (
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
)

func TestTrustedLabelersInheritanceAndReplacement(t *testing.T) {
	cfg, doc, err := config.Parse([]byte(`trusted_labelers: [Alice, release-bot]
repositories:
  - repo: owner/inherits
  - repo: owner/replaces
    trusted_labelers: [Bob]
  - repo: owner/defaults
    trusted_labelers: []
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) != 0 {
		t.Fatalf("recognized setting warned: %+v", doc.Warnings)
	}
	for i, want := range [][]string{{"Alice", "release-bot"}, {"Bob"}, {}} {
		if !reflect.DeepEqual(cfg.Repositories[i].TrustedLabelers, want) {
			t.Fatalf("repo %d: %v, want %v", i, cfg.Repositories[i].TrustedLabelers, want)
		}
	}
	cfg.Repositories[0].TrustedLabelers[0] = "changed"
	if cfg.TrustedLabelers[0] != "Alice" {
		t.Fatal("repository list aliases global list")
	}
}

func TestInvalidTrustedLabelers(t *testing.T) {
	for _, value := range []string{`null`, `[""]`, `[" alice "]`, `["../alice"]`, `["alice bob"]`, `alice`} {
		if _, _, err := config.Parse([]byte("trusted_labelers: " + value + "\n")); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestTrustedLabelersMustBeAListOfStrings(t *testing.T) {
	for _, value := range []string{`[123]`, `[true]`, `[null]`, `{alice: true}`} {
		if _, _, err := config.Parse([]byte("trusted_labelers: " + value + "\n")); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}
