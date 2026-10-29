package testkit

import (
	"context"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

type DeadlineBaseGit struct {
	*managedgit.Manager
	Calls int
	After func()
}

func (g *DeadlineBaseGit) CheckBase(ctx context.Context, run managedgit.Run, branch, head string) (managedgit.BaseMerge, error) {
	v, err := g.Manager.CheckBase(ctx, run, branch, head)
	g.Calls++
	if g.Calls == 2 {
		g.After()
	}
	return v, err
}
