package harness

import (
	"path/filepath"
	"strings"
)

// InteractiveCommand resumes a known conversation with normal interactive
// permissions. It contains no unattended environment, prompt, or result schema.
type InteractiveCommand struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
	Dir        string   `json:"dir"`
}

// ShellCommand is the same invocation for a user to copy into a POSIX shell.
func (c InteractiveCommand) ShellCommand() string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	words := []string{quote(c.Executable)}
	for _, arg := range c.Args {
		words = append(words, quote(arg))
	}
	return "cd " + quote(c.Dir) + " && " + strings.Join(words, " ")
}

func validateInteractive(sessionID, worktree string) error {
	if !sessionUUID.MatchString(sessionID) {
		return phaseError("takeover.session_invalid", "Takeover requires the implementer's exact session UUID; inspect the saved conversation", nil)
	}
	if !filepath.IsAbs(worktree) || strings.ContainsRune(worktree, 0) {
		return phaseError("takeover.worktree_missing", "Takeover requires a prepared worktree with an absolute path", nil)
	}
	return nil
}

func (c *Claude) BuildInteractiveInvocation(sessionID, worktree string) (InteractiveCommand, error) {
	if err := validateInteractive(sessionID, worktree); err != nil {
		return InteractiveCommand{}, err
	}
	return InteractiveCommand{Executable: c.config.Executable, Dir: worktree,
		Args: []string{"--resume", sessionID, "--permission-mode", "default"}}, nil
}

func (c *Codex) BuildInteractiveInvocation(sessionID, worktree string) (InteractiveCommand, error) {
	if err := validateInteractive(sessionID, worktree); err != nil {
		return InteractiveCommand{}, err
	}
	return InteractiveCommand{Executable: c.config.Executable, Dir: worktree,
		Args: []string{"resume", "-C", worktree, "-a", "on-request", "-s", "workspace-write", "--", sessionID}}, nil
}
