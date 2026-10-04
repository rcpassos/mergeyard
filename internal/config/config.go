package config

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/repository"
	"github.com/rcpassos/mergeyard/internal/workspace"
	"go.yaml.in/yaml/v3"
)

// Config is the effective configuration. Repository settings are fully resolved;
// an empty BaseBranch is intentionally left for runtime GitHub discovery.
type Config struct {
	Version      int
	Port         int
	OpenBrowser  bool
	Workspace    string
	PollInterval time.Duration
	Concurrency  int
	Labels       Labels
	Agents       Agents
	Implementer  Role
	Reviewer     Role
	MaxRounds    int
	PRComments   bool
	UsageLimits  UsageLimits
	Repositories []Repository
}

type Role struct {
	Agent       string
	Model       string // empty means the harness default
	Effort      string // empty means the harness default
	Skills      []string
	MaxAttempts int
}

type Labels struct {
	Ready          string
	Running        string
	NeedsAttention string
}

type Agents struct {
	Claude Claude
	Codex  Codex
}

type Claude struct {
	Executable     string
	PermissionMode string
	AllowedTools   []string
}

type Codex struct {
	Executable    string
	Sandbox       string
	NetworkAccess bool
}

type UsageLimits struct {
	Cooldown time.Duration
	MaxWaits int
}

type Repository struct {
	Repo        string
	BaseBranch  string
	Concurrency int
	Enabled     bool
	Implementer Role
	Reviewer    Role
	Labels      Labels
}

// Error retains the configuration API while sharing the runtime error shape.
type Error = fault.Error

func defaults() Config {
	role := Role{Agent: "claude", Skills: []string{}, MaxAttempts: 1}
	return Config{
		Version: 1, Port: 7331, OpenBrowser: true, Workspace: workspace.DefaultPath,
		PollInterval: 30 * time.Second, Concurrency: 1,
		Labels: Labels{Ready: "ready-for-agent", Running: "agent-running", NeedsAttention: "agent-needs-attention"},
		Agents: Agents{
			Claude: Claude{Executable: "claude", PermissionMode: "bypassPermissions", AllowedTools: []string{}},
			Codex:  Codex{Executable: "codex", Sandbox: "workspace-write", NetworkAccess: true},
		},
		Implementer: role, Reviewer: role, MaxRounds: 5, PRComments: true,
		UsageLimits: UsageLimits{Cooldown: 30 * time.Minute, MaxWaits: 3},
	}
}

// Document retains the source YAML rather than serializing resolved defaults.
type Document struct {
	Path     string    // selected source file; empty for Parse
	Warnings []Warning // diagnostics from Parse/Load; refresh by reloading after edits
	root     yaml.Node
}

// Warning reports a tolerated unknown field so callers can surface likely
// typos without rejecting future options or discarding their YAML.
type Warning struct {
	Code    string
	Path    string
	Message string
}

// Parse resolves one YAML file: defaults, then global fields, then repository
// overrides. "Global" means top-level fields in this file, not a second file.
// Unknown fields are retained in Document for forward-compatible editing.
func Parse(data []byte) (Config, *Document, error) {
	doc := &Document{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc.root); err != nil {
		return Config{}, nil, &Error{Code: "config.invalid_yaml", Path: "document", Err: err}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, nil, &Error{Code: "config.invalid_yaml", Path: "document", Err: fmt.Errorf("expected exactly one YAML document")}
	}
	cfg := defaults()
	var err error
	m := newMapping(doc.root.Content[0], "", &err, &doc.Warnings)
	m.read("version", &cfg.Version)
	m.read("port", &cfg.Port)
	m.read("open_browser", &cfg.OpenBrowser)
	m.read("workspace", &cfg.Workspace)
	m.readDuration("poll_interval", &cfg.PollInterval)
	m.readPositiveInt("concurrency", &cfg.Concurrency, "config.invalid_concurrency")
	readLabels(m.child("labels"), &cfg.Labels)
	agents := m.child("agents")
	claude := agents.child("claude")
	claude.read("executable", &cfg.Agents.Claude.Executable)
	claude.readEnum("permission_mode", &cfg.Agents.Claude.PermissionMode, "config.invalid_permission_mode", "auto", "acceptEdits", "bypassPermissions")
	claude.read("allowed_tools", &cfg.Agents.Claude.AllowedTools)
	claude.finish()
	codex := agents.child("codex")
	codex.read("executable", &cfg.Agents.Codex.Executable)
	codex.readEnum("sandbox", &cfg.Agents.Codex.Sandbox, "config.invalid_sandbox", "workspace-write", "danger-full-access")
	codex.read("network_access", &cfg.Agents.Codex.NetworkAccess)
	codex.finish()
	agents.finish()
	readRole(m.child("implementer"), &cfg.Implementer)
	readRole(m.child("reviewer"), &cfg.Reviewer)
	m.readPositiveInt("max_rounds", &cfg.MaxRounds, "config.invalid_max_rounds")
	m.read("pr_comments", &cfg.PRComments)
	usage := m.child("usage_limits")
	usage.readDuration("cooldown", &cfg.UsageLimits.Cooldown)
	usage.readPositiveInt("max_waits", &cfg.UsageLimits.MaxWaits, "config.invalid_max_waits")
	usage.finish()
	if repos := m.node("repositories"); repos != nil {
		repos = dereference(repos)
		if repos.Kind != yaml.SequenceNode {
			m.fail("config.invalid_yaml", "repositories", "expected a sequence")
		} else {
			seenRepos := make(map[string]bool)
			for i, node := range repos.Content {
				r := Repository{Concurrency: cfg.Concurrency, Enabled: true,
					Implementer: cloneRole(cfg.Implementer), Reviewer: cloneRole(cfg.Reviewer), Labels: cfg.Labels}
				rm := newMapping(node, fmt.Sprintf("repositories[%d]", i), &err, &doc.Warnings)
				rm.readRepo("repo", &r.Repo)
				if !repository.ValidName(r.Repo) {
					rm.fail("config.invalid_repo", "repo", "expected owner/repo")
				}
				name := strings.ToLower(r.Repo)
				if seenRepos[name] {
					rm.fail("config.duplicate_repo", "repo", "repository is already configured (case-insensitive)")
				}
				seenRepos[name] = true
				rm.read("base_branch", &r.BaseBranch)
				rm.readPositiveInt("concurrency", &r.Concurrency, "config.invalid_concurrency")
				rm.read("enabled", &r.Enabled)
				readRole(rm.child("implementer"), &r.Implementer)
				readRole(rm.child("reviewer"), &r.Reviewer)
				readLabels(rm.child("labels"), &r.Labels)
				rm.finish()
				cfg.Repositories = append(cfg.Repositories, r)
			}
		}
	}
	m.finish()
	if err != nil {
		return Config{}, nil, err
	}
	return cfg, doc, nil
}

func cloneRole(r Role) Role {
	r.Skills = slices.Clone(r.Skills)
	return r
}

func readRole(m mapping, r *Role) {
	m.readEnum("agent", &r.Agent, "config.invalid_agent", "claude", "codex")
	m.readNullableString("model", &r.Model)
	m.readNullableString("effort", &r.Effort)
	m.read("skills", &r.Skills)
	m.readPositiveInt("max_attempts", &r.MaxAttempts, "config.invalid_max_attempts")
	m.finish()
}

func readLabels(m mapping, l *Labels) {
	m.read("ready", &l.Ready)
	m.read("running", &l.Running)
	m.read("needs_attention", &l.NeedsAttention)
	seen := make(map[string]bool)
	for _, field := range []struct{ key, value string }{
		{"ready", l.Ready}, {"running", l.Running}, {"needs_attention", l.NeedsAttention},
	} {
		name := strings.ToLower(field.value)
		if strings.TrimSpace(field.value) == "" || seen[name] {
			m.fail("config.invalid_labels", field.key, "labels must be nonempty and distinct (case-insensitive)")
			return
		}
		seen[name] = true
	}
	m.finish()
}

// mapping applies only explicitly supplied fields, including false, empty
// lists, and null model/effort. All readers share the first decoding error.
type mapping struct {
	fields   map[string]*yaml.Node
	keys     []string
	known    map[string]bool
	warnings *[]Warning
	path     string
	err      *error
}

func newMapping(n *yaml.Node, path string, err *error, warnings *[]Warning) mapping {
	m := mapping{fields: make(map[string]*yaml.Node), known: make(map[string]bool), path: path, err: err, warnings: warnings}
	if n == nil || *err != nil {
		return m
	}
	n = dereference(n)
	if n.Kind != yaml.MappingNode {
		m.fail("config.invalid_yaml", "", "expected a mapping")
		return m
	}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Tag != "!!str" || m.fields[key.Value] != nil {
			m.fail("config.invalid_yaml", key.Value, "mapping keys must be unique strings; YAML merge keys are unsupported")
			return m
		}
		m.fields[key.Value] = n.Content[i+1]
		m.keys = append(m.keys, key.Value)
	}
	return m
}

func dereference(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func (m mapping) fieldPath(key string) string {
	if m.path == "" {
		return key
	}
	if key == "" {
		return m.path
	}
	return m.path + "." + key
}

func (m mapping) fail(code, key, message string) {
	if *m.err == nil {
		*m.err = &Error{Code: code, Path: m.fieldPath(key), Err: fmt.Errorf("%s", message)}
	}
}

func (m mapping) child(key string) mapping {
	return newMapping(m.node(key), m.fieldPath(key), m.err, m.warnings)
}

func (m mapping) finish() {
	if *m.err != nil {
		return
	}
	for _, key := range m.keys {
		if !m.known[key] {
			*m.warnings = append(*m.warnings, Warning{Code: "config.unknown_field", Path: m.fieldPath(key), Message: "unknown field is ignored when resolving configuration; check for a typo"})
		}
	}
}

func (m mapping) read(key string, target any) {
	n := m.node(key)
	if n == nil {
		return
	}
	if n.Tag == "!!null" {
		m.fail("config.invalid_yaml", key, "null is allowed only for model and effort")
		return
	}
	if err := n.Decode(target); err != nil {
		m.fail("config.invalid_yaml", key, err.Error())
	}
}

func (m mapping) node(key string) *yaml.Node {
	m.known[key] = true
	if n := m.fields[key]; n != nil && *m.err == nil {
		return dereference(n)
	}
	return nil
}

func (m mapping) readPositiveInt(key string, target *int, code string) {
	n := m.node(key)
	if n == nil {
		return
	}
	var value int
	if n.Tag != "!!int" || n.Decode(&value) != nil || value <= 0 {
		m.fail(code, key, "expected a positive finite integer")
		return
	}
	*target = value
}

func (m mapping) readDuration(key string, target *time.Duration) {
	n := m.node(key)
	if n == nil {
		return
	}
	if n.Tag != "!!str" {
		m.fail("config.invalid_duration", key, "expected a duration string such as 30s or 30m")
		return
	}
	value, err := time.ParseDuration(n.Value)
	if err != nil {
		m.fail("config.invalid_duration", key, err.Error())
		return
	}
	if value <= 0 {
		m.fail("config.invalid_duration", key, "expected a positive duration")
		return
	}
	*target = value
}

func (m mapping) readEnum(key string, target *string, code string, choices ...string) {
	n := m.node(key)
	if n == nil {
		return
	}
	if n.Tag != "!!str" || !slices.Contains(choices, n.Value) {
		m.fail(code, key, fmt.Sprintf("expected one of %v, got %q", choices, n.Value))
		return
	}
	*target = n.Value
}

func (m mapping) readNullableString(key string, target *string) {
	n := m.node(key)
	if n == nil {
		return
	}
	if n.Tag == "!!null" {
		*target = ""
		return
	}
	m.read(key, target)
}

func (m mapping) readRepo(key string, target *string) {
	n := m.node(key)
	if n == nil {
		return
	}
	if n.Tag != "!!str" {
		m.fail("config.invalid_repo", key, "expected owner/repo")
		return
	}
	*target = n.Value
}
