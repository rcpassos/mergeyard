package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// Set edits a mapping field or sequence entry. Paths use mapping keys and
// zero-based sequence indices; "-" appends to an existing sequence. Missing
// mapping parents are created, so init can start with Parse([]byte("{}")).
// Replacing a container intentionally replaces its children; edit individual
// leaves or append repositories to preserve their comments and unknown fields.
func (d *Document) Set(path []string, value any) error {
	invalid := func(message string) error {
		return &Error{Code: "config.invalid_path", Path: fmt.Sprint(path), Err: errors.New(message)}
	}
	if len(path) == 0 || len(d.root.Content) != 1 {
		return invalid("expected a nonempty path in a document")
	}
	for _, key := range path {
		if key == "" {
			return invalid("path keys cannot be empty")
		}
	}
	var replacement yaml.Node
	if err := replacement.Encode(value); err != nil {
		return &Error{Code: "config.invalid_yaml", Path: fmt.Sprint(path), Err: err}
	}
	n := d.root.Content[0]
	for i, key := range path {
		last := i == len(path)-1
		var next *yaml.Node
		switch n.Kind {
		case yaml.MappingNode:
			for j := 0; j < len(n.Content); j += 2 {
				if n.Content[j].Value == key {
					next = n.Content[j+1]
					break
				}
			}
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, next)
			}
		case yaml.SequenceNode:
			if key == "-" && last {
				n.Content = append(n.Content, &replacement)
				return nil
			}
			index, err := strconv.Atoi(key)
			if err != nil || index < 0 || index >= len(n.Content) {
				return invalid("sequence index out of range")
			}
			next = n.Content[index]
		default:
			return invalid("cannot traverse a scalar or alias; edit the anchor directly")
		}
		if last {
			if next.Kind == yaml.AliasNode {
				return invalid("cannot replace an alias; edit the anchor directly")
			}
			replacement.HeadComment = next.HeadComment
			replacement.LineComment = next.LineComment
			replacement.FootComment = next.FootComment
			replacement.Anchor = next.Anchor
			if next.Kind == replacement.Kind && next.Tag == replacement.Tag {
				replacement.Style = next.Style
			}
			*next = replacement
			return nil
		}
		n = next
	}
	return nil
}

// Write validates the edited YAML before atomically replacing path. New files
// use mode 0600; existing permission bits are preserved. Comments, unknown
// fields, key order and unchanged node styles survive, but whitespace may be
// normalized by the YAML encoder. Concurrent external edits are not merged.
func (d *Document) Write(path string) error {
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&d.root); err != nil {
		return &Error{Code: "config.write_failed", Path: path, Err: err}
	}
	if err := encoder.Close(); err != nil {
		return &Error{Code: "config.write_failed", Path: path, Err: err}
	}
	if _, _, err := Parse(buf.Bytes()); err != nil {
		return err
	}
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return &Error{Code: "config.write_failed", Path: path, Err: err}
	}
	d.Path = path
	return nil
}

func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0600)
	info, err := os.Stat(path)
	if err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mergeyard-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
