package config

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

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

// Error provides a stable code and a field path for CLI/UI callers.
type Error struct {
	Code string
	Path string
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s: %v", e.Code, e.Path, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

func defaults() Config {
	role := Role{Agent: "claude", Skills: []string{}, MaxAttempts: 1}
	return Config{
		Version: 1, Port: 7331, OpenBrowser: true, Workspace: "~/.mergeyard",
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
	Path string // selected source file; empty for Parse
	root yaml.Node
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
	m := newMapping(doc.root.Content[0], "", &err)
	m.read("version", &cfg.Version)
	m.read("port", &cfg.Port)
	m.read("open_browser", &cfg.OpenBrowser)
	m.read("workspace", &cfg.Workspace)
	m.read("poll_interval", &cfg.PollInterval)
	m.read("concurrency", &cfg.Concurrency)
	readLabels(m.child("labels"), &cfg.Labels)
	claude := m.child("agents").child("claude")
	claude.read("executable", &cfg.Agents.Claude.Executable)
	claude.read("permission_mode", &cfg.Agents.Claude.PermissionMode)
	claude.read("allowed_tools", &cfg.Agents.Claude.AllowedTools)
	codex := m.child("agents").child("codex")
	codex.read("executable", &cfg.Agents.Codex.Executable)
	codex.read("sandbox", &cfg.Agents.Codex.Sandbox)
	codex.read("network_access", &cfg.Agents.Codex.NetworkAccess)
	readRole(m.child("implementer"), &cfg.Implementer)
	readRole(m.child("reviewer"), &cfg.Reviewer)
	m.read("max_rounds", &cfg.MaxRounds)
	m.read("pr_comments", &cfg.PRComments)
	usage := m.child("usage_limits")
	usage.read("cooldown", &cfg.UsageLimits.Cooldown)
	usage.read("max_waits", &cfg.UsageLimits.MaxWaits)
	if repos := m.fields["repositories"]; repos != nil && err == nil {
		repos = dereference(repos)
		if repos.Kind != yaml.SequenceNode {
			m.fail("config.invalid_yaml", "repositories", "expected a sequence")
		} else {
			for i, node := range repos.Content {
				r := Repository{Concurrency: cfg.Concurrency, Enabled: true,
					Implementer: cloneRole(cfg.Implementer), Reviewer: cloneRole(cfg.Reviewer), Labels: cfg.Labels}
				rm := newMapping(node, fmt.Sprintf("repositories[%d]", i), &err)
				rm.read("repo", &r.Repo)
				if !validRepo(r.Repo) {
					rm.fail("config.invalid_repo", "repo", "expected owner/repo")
				}
				rm.read("base_branch", &r.BaseBranch)
				rm.read("concurrency", &r.Concurrency)
				rm.read("enabled", &r.Enabled)
				readRole(rm.child("implementer"), &r.Implementer)
				readRole(rm.child("reviewer"), &r.Reviewer)
				readLabels(rm.child("labels"), &r.Labels)
				cfg.Repositories = append(cfg.Repositories, r)
			}
		}
	}
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
	m.read("agent", &r.Agent)
	m.read("model", &r.Model)
	m.read("effort", &r.Effort)
	m.read("skills", &r.Skills)
	m.read("max_attempts", &r.MaxAttempts)
}

func readLabels(m mapping, l *Labels) {
	m.read("ready", &l.Ready)
	m.read("running", &l.Running)
	m.read("needs_attention", &l.NeedsAttention)
}

// mapping applies only explicitly supplied fields, including false, empty
// lists, and null model/effort. All readers share the first decoding error.
type mapping struct {
	fields map[string]*yaml.Node
	path   string
	err    *error
}

func newMapping(n *yaml.Node, path string, err *error) mapping {
	m := mapping{fields: make(map[string]*yaml.Node), path: path, err: err}
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
	return newMapping(m.fields[key], m.fieldPath(key), m.err)
}

func (m mapping) read(key string, target any) {
	n := m.fields[key]
	if n == nil || *m.err != nil {
		return
	}
	n = dereference(n)
	code := "config.invalid_yaml"
	switch key {
	case "concurrency", "max_rounds", "max_attempts", "max_waits":
		code = "config.invalid_" + key
		var value int
		if n.Tag != "!!int" || n.Decode(&value) != nil || value <= 0 {
			m.fail(code, key, "expected a positive finite integer")
			return
		}
		*target.(*int) = value
		return
	case "poll_interval", "cooldown":
		if n.Tag != "!!str" {
			m.fail("config.invalid_duration", key, "expected a duration string such as 30s or 30m")
			return
		}
		value, err := time.ParseDuration(n.Value)
		if err != nil {
			m.fail("config.invalid_duration", key, err.Error())
			return
		}
		*target.(*time.Duration) = value
		return
	case "repo", "agent", "permission_mode", "sandbox":
		code = "config.invalid_" + key
		valid := n.Tag == "!!str"
		switch key {
		case "repo":
			valid = valid && validRepo(n.Value)
		case "agent":
			valid = valid && slices.Contains([]string{"claude", "codex"}, n.Value)
		case "permission_mode":
			valid = valid && slices.Contains([]string{"auto", "acceptEdits", "bypassPermissions"}, n.Value)
		case "sandbox":
			valid = valid && slices.Contains([]string{"workspace-write", "danger-full-access"}, n.Value)
		}
		if !valid {
			m.fail(code, key, fmt.Sprintf("unsupported value %q", n.Value))
			return
		}
	}
	if n.Tag == "!!null" {
		if key == "model" || key == "effort" {
			*target.(*string) = ""
			return
		}
		m.fail("config.invalid_yaml", key, "null is allowed only for model and effort")
		return
	}
	if err := n.Decode(target); err != nil {
		m.fail(code, key, err.Error())
	}
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$`)

func validRepo(repo string) bool {
	_, name, _ := strings.Cut(repo, "/")
	return repoPattern.MatchString(repo) && name != "." && name != ".."
}
