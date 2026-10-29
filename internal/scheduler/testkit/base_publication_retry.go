package testkit

import (
	"context"

	"github.com/rcpassos/mergeyard/internal/fault"
	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

type InterruptedBasePush struct {
	*managedgit.Manager
	Fail    bool
	Applied bool
}

func (g *InterruptedBasePush) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	if g.Fail {
		g.Fail = false
		if g.Applied {
			if err := g.Manager.PushFix(ctx, run, previous, target); err != nil {
				return err
			}
		}
		return &fault.Error{Code: "git.push_failed", Message: "simulated interrupted publication"}
	}
	return g.Manager.PushFix(ctx, run, previous, target)
}
