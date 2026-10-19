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
	"github.com/rcpassos/mergeyard/internal/runtimeauth"
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
	// Rotate the runtime secret on every start so a secret read before the
	// restart can no longer authenticate a client, and recreate it if it was
	// removed. The CLI re-reads the file for each command.
	if err := runtimeauth.RotateSecret(owner.Workspace.Root); err != nil {
		return err
	}
	report := doctor.Check(ctx, doc.Path, doctor.Options{})
	if len(report.Findings) > 0 {
		if err := report.Write(stderr); err != nil {
			return err
		}
	}
	if blockingDoctorErrors(report, cfg) {
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
	reconcileReport, reconcileErr := engine.Reconcile(ctx)
	for _, finding := range reconcileReport.Findings {
		if _, err := fmt.Fprintln(stderr, finding); err != nil {
			return errors.Join(reconcileErr, err)
		}
	}
	if reconcileErr != nil {
		// A GitHub failure at boot is the same scoped error ticks log and retry,
		// so it must not stop the dashboard even when the repository is missing
		// or forbidden. Local state that is unsafe to dispatch on still refuses
		// to start.
		if !scheduler.GitHubFailure(reconcileErr) {
			return fmt.Errorf("reconcile existing runs: %w", reconcileErr)
		}
		if _, err := fmt.Fprintf(stderr, "mergeyard: startup reconcile deferred to later ticks: %v\n", reconcileErr); err != nil {
			return err
		}
	}
	dashboard, err := web.NewDashboard(owner.Events, owner.Scheduler, web.DashboardOptions{
		DB: owner.DB, Workspace: owner.Workspace, Scheduler: engine, Config: cfg, ConfigPath: doc.Path,
		Diagnostics: func(context.Context) doctor.Report { return report },
	})
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
	updatesDone := make(chan struct{})
	go func() { defer close(updatesDone); dashboard.RunUpdates(liveCtx) }()
	address := "http://" + listener.Addr().String()
	// The dashboard is locked to the workspace owner; the printed link carries
	// a single-use token that sets the owner session cookie. mergeyard open
	// mints a fresh link when this one expires.
	dashboardURL := address + "/?token=" + dashboard.NewDashboardToken()
	fmt.Fprintf(stdout, "mergeyard: workspace ready at %s; dashboard at %s\n", owner.Workspace.Root, dashboardURL)
	// Browser launch is optional and must not delay HTTP controls. Keep it
	// owned by the runtime so shutdown cancels and reaps a blocked launcher.
	browserDone := make(chan struct{})
	if cfg.OpenBrowser {
		go func() {
			defer close(browserDone)
			if err := openBrowser(liveCtx, dashboardURL); err != nil && liveCtx.Err() == nil {
				fmt.Fprintf(stderr, "mergeyard: could not open browser: %v\n", err)
			}
		}()
	} else {
		close(browserDone)
	}
	serveErr := dashboard.Serve(liveCtx, listener)
	cancel()
	schedulerErr := <-schedulerDone
	<-updatesDone
	<-browserDone
	if errors.Is(schedulerErr, context.Canceled) {
		schedulerErr = nil
	}
	return errors.Join(serveErr, schedulerErr, owner.Close())
}

// blockingDoctorErrors ignores access failures for one configured repository.
// Startup can serve and let reconciliation report a deleted, renamed, or
// forbidden repository while dispatch continues elsewhere. Machine, local
// workspace, and repository-content errors still refuse to start.
func blockingDoctorErrors(report doctor.Report, cfg config.Config) bool {
	for _, finding := range report.Findings {
		if finding.Severity != doctor.Error {
			continue
		}
		if finding.RepositoryScoped(cfg.Repositories) && repositoryAccessFailure(finding.Code) {
			continue
		}
		return true
	}
	return false
}

func repositoryAccessFailure(code string) bool {
	switch code {
	case "github.labels_check_failed", "github.push_forbidden", "git.origin_inaccessible":
		return true
	}
	return false
}

func openBrowser(ctx context.Context, address string) error {
	executable := "xdg-open"
	if runtime.GOOS == "darwin" {
		executable = "open"
	}
	return exec.CommandContext(ctx, executable, address).Run()
}
