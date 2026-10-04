package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Load selects an explicit path, or the first existing file in the PRD search
// order. An existing but unreadable or invalid file never falls through.
func Load(path string) (Config, *Document, error) {
	if path != "" {
		return loadFile(path)
	}
	cfg, doc, err := loadFile("mergeyard.yaml")
	if !errors.Is(err, os.ErrNotExist) {
		return cfg, doc, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, nil, &Error{Code: "config.read_failed", Path: "~/.config/mergeyard/config.yaml", Err: err}
	}
	path = filepath.Join(home, ".config", "mergeyard", "config.yaml")
	cfg, doc, err = loadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = &Error{Code: "config.not_found", Path: path, Err: fmt.Errorf("no config at ./mergeyard.yaml or %s: %w", path, os.ErrNotExist)}
	}
	return cfg, doc, err
}

func loadFile(path string) (Config, *Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		code := "config.read_failed"
		if errors.Is(err, os.ErrNotExist) {
			code = "config.not_found"
		}
		return Config{}, nil, &Error{Code: code, Path: path, Err: err}
	}
	cfg, doc, err := Parse(data)
	if err != nil {
		return Config{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	doc.Path = path
	return cfg, doc, nil
}
