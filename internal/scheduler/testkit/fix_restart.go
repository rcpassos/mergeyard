package testkit

import (
	"context"
	"fmt"

	managedgit "github.com/rcpassos/mergeyard/internal/git"
)

type InterruptedFixGit struct {
	*managedgit.Manager
	Stage string
}

func (g InterruptedFixGit) CommitFix(ctx context.Context, run managedgit.Run, phase managedgit.Phase, target string) (managedgit.CommitResult, error) {
	result, err := g.Manager.CommitFix(ctx, run, phase, target)
	if err == nil && g.Stage == "commit" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return result, err
}
func (g InterruptedFixGit) PushFix(ctx context.Context, run managedgit.Run, previous, target string) error {
	err := g.Manager.PushFix(ctx, run, previous, target)
	if err == nil && g.Stage == "push" {
		fmt.Println("review-control-plane-ready")
		select {}
	}
	return err
}
