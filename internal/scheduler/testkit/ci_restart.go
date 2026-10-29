package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
)

type CiProcessInput struct {
	Config                  config.Config
	Workspace, ExternalPath string
	PR                      *github.PullRequest
	Now                     time.Time
	DuringReady             bool
}
type InterruptedReadyGitHub struct {
	*CiGitHub
	Path string
}

func (g InterruptedReadyGitHub) MarkReady(_ context.Context, _ string, _ int) error {
	pr := g.Prs["mergeyard/issue-7"]
	pr.Draft = false
	data, err := json.Marshal(pr)
	if err != nil {
		return err
	}
	if err := os.WriteFile(g.Path, data, 0600); err != nil {
		return err
	}
	fmt.Println("ci-control-plane-ready")
	select {} // Parent kills after the remote write, before its local acknowledgement.
}
