package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"syscall"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/github"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/store"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

func plan(ctx context.Context, configPath, repo string, asJSON bool, out io.Writer) error {
	client, _, err := connect(ctx, configPath)
	var report scheduler.Plan
	if err == nil {
		defer client.http.CloseIdleConnections()
		err = client.request(ctx, http.MethodGet, "/api/plan?repo="+url.QueryEscape(repo), &report, nil)
	} else if errors.Is(err, syscall.ECONNREFUSED) {
		// Fall back only when there is no listener. An active but unresponsive,
		// incompatible, or mismatched control plane must never be silently bypassed.
		cfg, _, loadErr := config.Load(configPath)
		if loadErr != nil {
			return loadErr
		}
		root, pathErr := workspace.ResolvePath(cfg.Workspace)
		if pathErr != nil {
			return pathErr
		}
		path := (&workspace.Workspace{Root: root}).DatabasePath()
		var db *sql.DB
		if _, statErr := os.Stat(path); statErr == nil {
			db, err = store.OpenReadOnly(ctx, path)
			if err != nil {
				return err
			}
			defer db.Close()
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		report, err = scheduler.PlanOffline(ctx, cfg, db, scheduler.Dependencies{GitHub: github.New(nil)}, repo)
	}
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	if report.Paused {
		fmt.Fprintln(out, "Scheduler paused; new claims are waiting.")
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(out, "%s#%d %s", terminalText(issue.Repository), issue.Number, issue.Decision)
		if issue.Reason != "" {
			fmt.Fprintf(out, ": %s", issue.Reason)
		}
		for _, blocker := range issue.Blockers {
			fmt.Fprintf(out, " #%d", blocker)
		}
		fmt.Fprintf(out, " · %s\n", terminalText(issue.Title))
		if issue.RoleSettings != nil {
			printStoredRole(out, "implementer", issue.RoleSettings.Implementer)
			printStoredRole(out, "reviewer", issue.RoleSettings.Reviewer)
		}
	}
	if len(report.Issues) == 0 {
		fmt.Fprintln(out, "No open ready issues.")
	}
	return nil
}
