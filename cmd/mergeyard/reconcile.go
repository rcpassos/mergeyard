package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/scheduler"
)

func reconcile(ctx context.Context, configPath string, out io.Writer) (err error) {
	cfg, _, err := config.Load(configPath)
	if err != nil {
		return err
	}
	runtime, err := app.Open(ctx, cfg.Workspace)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, runtime.Close()) }()
	env := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	s, err := scheduler.New(cfg, scheduler.Resources{DB: runtime.DB, Events: runtime.Events, Workflow: runtime.Workflow, Workspace: runtime.Workspace, Control: runtime.Scheduler}, scheduler.Dependencies{Env: env})
	if err != nil {
		return err
	}
	report, reconcileErr := s.Reconcile(ctx)
	for _, finding := range report.Findings {
		if _, err := fmt.Fprintln(out, finding); err != nil {
			return errors.Join(reconcileErr, err)
		}
	}
	if reconcileErr != nil {
		return reconcileErr
	}
	_, err = fmt.Fprintf(out, "Reconciliation complete: %d finding(s).\n", len(report.Findings))
	return err
}
