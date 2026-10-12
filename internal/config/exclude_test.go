package config_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/rcpassos/mergeyard/internal/config"
)

func TestRepositoryExcludePatterns(t *testing.T) {
	cfg, doc, err := config.Parse([]byte(`repositories:
  - repo: owner/repo
    exclude: [".venv/", "*.har", "**/traces/**"]
  - repo: owner/other
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) != 0 || !reflect.DeepEqual(cfg.Repositories[0].Exclude, []string{".venv/", "*.har", "**/traces/**"}) || len(cfg.Repositories[1].Exclude) != 0 {
		t.Fatalf("exclusions not resolved per repository: %+v, %+v", cfg.Repositories, doc.Warnings)
	}
}

func TestInvalidRepositoryExcludePatterns(t *testing.T) {
	for _, value := range []string{`[""]`, `[" "]`, `["["]`, `["trailing\\"]`, `["ab**cd"]`, `["***"]`, `["../outside"]`, `["/absolute"]`, `["nul\0"]`, `[123]`, `null`, `"*.har"`} {
		t.Run(value, func(t *testing.T) {
			_, _, err := config.Parse([]byte("repositories: [{repo: owner/repo, exclude: " + value + "}]"))
			var failure *config.Error
			if !errors.As(err, &failure) || failure.Code != "config.invalid_exclude" || failure.Path != "repositories[0].exclude" {
				t.Fatalf("invalid exclude %s: %v", value, err)
			}
		})
	}
}
