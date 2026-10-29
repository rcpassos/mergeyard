package testkit

import (
	"context"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

// Count publication effects at the real Git adapter boundary: repairing labels
// must not re-inspect, re-commit or re-push an accepted handback.
type HandbackLabelGit struct {
	*managedgit.Manager
	Inspected, Committed, Pushed int
}

func (g *HandbackLabelGit) InspectHandback(ctx context.Context, run managedgit.Run, target string) (string, error) {
	g.Inspected++
	return g.Manager.InspectHandback(ctx, run, target)
}
func (g *HandbackLabelGit) CommitHandback(ctx context.Context, run managedgit.Run, previous string) (managedgit.CommitResult, error) {
	g.Committed++
	return g.Manager.CommitHandback(ctx, run, previous)
}
func (g *HandbackLabelGit) PushHandback(ctx context.Context, run managedgit.Run, previous, target string) error {
	g.Pushed++
	return g.Manager.PushHandback(ctx, run, previous, target)
}
