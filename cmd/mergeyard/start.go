package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/rcpassos/mergeyard/internal/app"
	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/doctor"
	"github.com/rcpassos/mergeyard/internal/scheduler"
	"github.com/rcpassos/mergeyard/internal/web"
)

func start(path string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, path, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "mergeyard start: %v\n", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, path string, stdout, stderr io.Writer) error {
	cfg, doc, err := config.Load(path)
	if err != nil {
		return err
	}
	owner, err := app.Open(ctx, cfg.Workspace)
	if err != nil {
		return err
	}
	defer owner.Close()
	report := doctor.Check(ctx, doc.Path, doctor.Options{})
	if len(report.Findings) > 0 {
		if err := report.Write(stderr); err != nil {
			return err
		}
	}
	if report.HasErrors() {
		return errors.New("dependency checks failed; run mergeyard doctor")
	}
	env := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}
	engine, err := scheduler.New(cfg, scheduler.Resources{DB: owner.DB, Events: owner.Events, Workflow: owner.Workflow, Workspace: owner.Workspace, Control: owner.Scheduler}, scheduler.Dependencies{Env: env})
	if err != nil {
		return err
	}
	if err := engine.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile existing runs: %w", err)
	}
	dashboard, err := web.NewWithOperations(owner.Events, owner.Scheduler, engine, owner.Workspace.Root)
	if err != nil {
		return err
	}
	listener, err := web.Listen(fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return err
	}
	defer listener.Close()
	liveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- engine.Run(liveCtx) }()
	address := "http://" + listener.Addr().String()
	fmt.Fprintf(stdout, "mergeyard: workspace ready at %s; dashboard at %s\n", owner.Workspace.Root, address)
	if cfg.OpenBrowser {
		if err := openBrowser(liveCtx, address); err != nil {
			fmt.Fprintf(stderr, "mergeyard: could not open browser: %v\n", err)
		}
	}
	serveErr := dashboard.Serve(liveCtx, listener)
	cancel()
	schedulerErr := <-schedulerDone
	if errors.Is(schedulerErr, context.Canceled) {
		schedulerErr = nil
	}
	return errors.Join(serveErr, schedulerErr, owner.Close())
}

func openBrowser(ctx context.Context, address string) error {
	executable := "xdg-open"
	if runtime.GOOS == "darwin" {
		executable = "open"
	}
	return exec.CommandContext(ctx, executable, address).Run()
}
