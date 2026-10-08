package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rcpassos/mergeyard/internal/config"
	"github.com/rcpassos/mergeyard/internal/fault"
	"github.com/rcpassos/mergeyard/internal/harness"
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/web"
	"github.com/rcpassos/mergeyard/internal/workflow"
	"github.com/rcpassos/mergeyard/internal/workspace"
)

// runtimeClient only connects to the configured loopback port. It never follows
// redirects or ambient HTTP proxies, and verifies the workspace before controls.
type runtimeClient struct {
	base, token string
	http        *http.Client
}

func connect(ctx context.Context, path string) (*runtimeClient, web.Status, error) {
	cfg, _, err := config.Load(path)
	if err != nil {
		return nil, web.Status{}, err
	}
	client := &runtimeClient{base: fmt.Sprintf("http://127.0.0.1:%d", cfg.Port), http: &http.Client{
		Timeout:       30 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}
	var status web.Status
	if err := client.request(ctx, http.MethodGet, "/api/status", &status); err != nil {
		return nil, status, err
	}
	root, err := workspace.ResolvePath(cfg.Workspace)
	if err != nil {
		return nil, status, err
	}
	if root != status.Workspace {
		return nil, status, &fault.Error{Code: "workspace.mismatch", Message: "Configured port belongs to a different workspace"}
	}
	client.token = status.Token
	return client, status, nil
}

func (c *runtimeClient) request(ctx context.Context, method, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Origin", c.base)
		req.Header.Set("X-CSRF-Token", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &fault.Error{Code: "internal.runtime_unavailable", Message: "Cannot reach Mergeyard; start it with the same configuration", Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("local API (%s): %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(result)
	}
	return nil
}

func operate(ctx context.Context, path, action string, args []string, stdout, stderr io.Writer) error {
	client, status, err := connect(ctx, path)
	if err != nil {
		return err
	}
	defer client.http.CloseIdleConnections()
	switch action {
	case "status":
		state := "running"
		if status.Paused {
			state = "paused"
		}
		fmt.Fprintf(stdout, "Scheduler: %s\n", state)
		count := 0
		for _, v := range status.Harnesses {
			state := "available"
			if !v.Available {
				state = "waiting until " + v.ResetAt.Format(time.RFC3339) + " (" + v.ResetTimeSource + "; " + v.Source + ")"
			}
			fmt.Fprintf(stdout, "Harness %s: %s\n", terminalText(v.Harness), terminalText(state))
		}
		for _, run := range status.Runs {
			if run.State.Terminal() && run.State != workflow.Failed && !(run.Merge != nil && run.Merge.Pending()) {
				continue
			}
			if !run.State.Terminal() {
				count++
			}
			fmt.Fprintf(stdout, "%s  %s#%d  %s/%s\n", terminalText(run.ID), terminalText(run.Repository), run.IssueNumber, terminalText(string(run.State)), terminalText(string(run.Phase)))
			if run.TakeoverStatus == workflow.TakeoverRequested {
				fmt.Fprintln(stdout, "  Takeover requested; automation is suspended while processes stop and review restoration finishes")
			}
			if run.State == workflow.Manual {
				fmt.Fprintf(stdout, "  Resume the exact implementer: mergeyard takeover %s\n", terminalText(run.ID))
			}
			if run.Merge != nil {
				if run.Merge.Pending() {
					fmt.Fprintln(stdout, "  Merged — cleanup pending")
				} else {
					fmt.Fprintln(stdout, "  Merged — cleanup complete")
				}
				if run.Merge.Early {
					fmt.Fprintln(stdout, "  Early merge before readiness")
				}
				for _, action := range run.Merge.Remaining() {
					fmt.Fprintf(stdout, "    %s\n", terminalText(action))
				}
				if run.Merge.Error != "" {
					fmt.Fprintf(stdout, "  %s\n", terminalText(run.Merge.Error))
				}
			}
			if run.LastErrorCode != "" {
				fmt.Fprintf(stdout, "  %s: %s\n", terminalText(run.LastErrorCode), terminalText(run.LastErrorMessage))
			}
			if run.State == workflow.NeedsAttention || run.State == workflow.Failed {
				fmt.Fprintf(stdout, "  Retry: mergeyard retry %s (reconciles preserved work first)\n", terminalText(run.ID))
			}
			if run.State == workflow.WaitingForHarness && run.HarnessWait != nil {
				v := run.HarnessWait
				fmt.Fprintf(stdout, "  Waiting for harness %s until %s · %s · %s\n", terminalText(v.Harness), v.ResetAt.Format(time.RFC3339), terminalText(v.ResetTimeSource), terminalText(v.Source))
			}
			for _, v := range run.HarnessWaitHistory {
				printHarnessWait(stdout, v)
			}
			for _, v := range run.Handbacks {
				printHandback(stdout, v)
			}
			for _, v := range run.Retries {
				printRetry(stdout, v)
			}
			if run.State == workflow.ReadyToMerge {
				fmt.Fprintln(stdout, "  Waiting for your merge")
			}
			if v := run.CI; v != nil {
				fmt.Fprintf(stdout, "  CI wait %s → %s · approved %s · current head %s\n", v.StartedAt.Format(time.RFC3339), v.Deadline.Format(time.RFC3339), terminalText(v.SHA), terminalText(v.CurrentHead))
				if v.RepairCause != "" {
					fmt.Fprintf(stdout, "  CI repair cause: %s\n", terminalText(v.RepairCause))
				}
				if v.Evidence.MergeSHA != "" {
					fmt.Fprintf(stdout, "  verified test merge commit %s\n", terminalText(v.Evidence.MergeSHA))
				}
				if v.QueryError != "" {
					fmt.Fprintf(stdout, "  CI evidence unknown: %s\n", terminalText(v.QueryError))
				}
				if v.Warning != "" {
					fmt.Fprintf(stdout, "  %s\n", terminalText(v.Warning))
				}
				for _, c := range v.Evidence.Checks {
					fmt.Fprintf(stdout, "    %s: %s (%s/%s) app %d · commit %s %s\n", terminalText(c.Name), terminalText(c.State), terminalText(c.Status), terminalText(c.Conclusion), c.AppID, terminalText(c.SHA), terminalText(c.URL))
					if c.Excerpt != "" {
						fmt.Fprintf(stdout, "      %s\n", terminalText(c.Excerpt))
					}
				}
				for _, r := range v.Evidence.Required {
					fmt.Fprintf(stdout, "    required %s (app %d)\n", terminalText(r.Name), r.AppID)
				}
			}
			for _, v := range run.SessionRecoveries {
				fmt.Fprintf(stdout, "  harness.session_resume_failed: %s round %d · saved session %s is missing; continuing once in a fresh session · %s active session %s\n", terminalText(string(v.Phase)), v.Round, terminalText(v.PreviousSessionID), terminalText(v.Role), terminalText(v.SessionID))
			}
			if v := run.Implementer; v != nil {
				fmt.Fprintf(stdout, "  implementer %s · model %s · effort %s · skills %s · permissions %s · session %s\n", terminalText(v.Agent), terminalText(v.Model), terminalText(v.Effort), terminalText(strings.Join(v.Skills, ",")), terminalText(v.Permissions), terminalText(v.SessionID))
				fmt.Fprintf(stdout, "  implement attempt %d: %s · process %s\n", v.Attempt, terminalText(v.Status), terminalText(v.ProcessSession))
			}
			for _, v := range run.FixHistory {
				fmt.Fprintf(stdout, "  fixer %s · model %s · effort %s · skills %s · permissions %s · session %s\n", terminalText(v.Agent), terminalText(v.Model), terminalText(v.Effort), terminalText(strings.Join(v.Skills, ",")), terminalText(v.PermissionMode), terminalText(v.SessionID))
				fmt.Fprintf(stdout, "  fix round %d attempt %d: %s · session %s · target %s · commit %s · pushed=%t\n", v.Round, v.Attempt, terminalText(v.Status), terminalText(v.SessionID), terminalText(v.TargetSHA), terminalText(v.CommitSHA), v.Pushed)
				if v.CI != nil {
					fmt.Fprintf(stdout, "    CI repair cause: %s · next independent review round %d\n", terminalText(v.CI.RepairCause), v.Round+1)
					for _, c := range v.CI.Evidence.Checks {
						fmt.Fprintf(stdout, "    %s: %s · %s · %s\n", terminalText(c.Name), terminalText(c.Conclusion), terminalText(c.Excerpt), terminalText(c.URL))
					}
				}
				for _, f := range v.Findings {
					fmt.Fprintf(stdout, "    finding %s: %s — %s\n", terminalText(f.ID), terminalText(f.Title), terminalText(f.Details))
				}
				if v.Report != nil {
					fmt.Fprintf(stdout, "  fix %s: %s\n", terminalText(v.Report.Status), terminalText(v.Report.Summary))
					for _, r := range v.Report.Responses {
						fmt.Fprintf(stdout, "    %s %s: %s\n", terminalText(r.FindingID), terminalText(r.Resolution), terminalText(r.Note))
					}
				}
				if v.Error != "" {
					fmt.Fprintf(stdout, "  fix error: %s\n", terminalText(v.Error))
				}
			}
			if v := run.Review; v != nil {
				fmt.Fprintf(stdout, "  reviewer %s · model %s · effort %s · skills %s · permissions %s · session %s\n", terminalText(v.Agent), terminalText(v.Model), terminalText(v.Effort), terminalText(strings.Join(v.Skills, ",")), terminalText(v.PermissionMode), terminalText(v.SessionID))
				fmt.Fprintf(stdout, "  review round %d attempt %d: %s · target %s\n", v.Round, v.Attempt, terminalText(v.Status), terminalText(v.TargetSHA))
				if v.Report != nil {
					fmt.Fprintf(stdout, "  verdict %s (accepted=%t): %s\n", terminalText(v.Report.Status), v.Accepted, terminalText(v.Report.Summary))
					for _, f := range v.Report.Findings {
						fmt.Fprintf(stdout, "    %s %s: %s — %s", terminalText(f.ID), terminalText(f.Severity), terminalText(f.Title), terminalText(f.Details))
						if f.File != nil {
							fmt.Fprintf(stdout, " (%s", terminalText(*f.File))
							if f.Line != nil {
								fmt.Fprintf(stdout, ":%d", *f.Line)
							}
							fmt.Fprint(stdout, ")")
						}
						fmt.Fprintln(stdout)
					}
				}
			}
		}
		if count == 0 {
			fmt.Fprintln(stdout, "No active runs.")
		}
	case "pause", "resume":
		if err := client.request(ctx, http.MethodPost, "/scheduler/"+action, nil); err != nil {
			return err
		}
		state := "paused"
		if action == "resume" {
			state = "resumed"
		}
		fmt.Fprintf(stdout, "Scheduler %s.\n", state)
	case "stop":
		if err := client.request(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(args[0])+"/stop", nil); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Run %s stopped.\n", args[0])
	case "retry":
		var run workflow.Run
		if err := client.request(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(args[0])+"/retry", &run); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Run %s: %s/%s\n", terminalText(run.ID), terminalText(string(run.State)), terminalText(string(run.Phase)))
		if len(run.Retries) > 0 {
			printRetry(stdout, run.Retries[len(run.Retries)-1])
		}
		if run.LastErrorCode != "" {
			fmt.Fprintf(stdout, "  %s: %s\n", terminalText(run.LastErrorCode), terminalText(run.LastErrorMessage))
		}
	case "handback":
		fmt.Fprintln(stdout, "Handback commits and pushes manual work. Exit the interactive harness first. If independent review exceeds the allowance, exactly one additional review round will be granted.")
		var run workflow.Run
		if err := client.request(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(args[0])+"/handback", &run); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Run %s: %s/%s\n", terminalText(run.ID), terminalText(string(run.State)), terminalText(string(run.Phase)))
		if len(run.Handbacks) > 0 {
			printHandback(stdout, run.Handbacks[len(run.Handbacks)-1])
		}
	case "takeover":
		return runner.RunInteractivePrepared(ctx, status.Workspace, args[0], func(ctx context.Context) (runner.ExecRequest, error) {
			var command harness.InteractiveCommand
			if err := client.request(ctx, http.MethodPost, "/api/runs/"+url.PathEscape(args[0])+"/takeover", &command); err != nil {
				return runner.ExecRequest{}, err
			}
			fmt.Fprintf(stdout, "Run %s is MANUAL. Exit the interactive harness before handing control back.\n", terminalText(args[0]))
			return runner.ExecRequest{Executable: command.Executable, Args: command.Args, Dir: command.Dir}, nil
		}, os.Stdin, stdout, stderr)
	case "watch":
		var ref runner.SessionRef
		if err := client.request(ctx, http.MethodGet, "/api/runs/"+url.PathEscape(args[0])+"/watch", &ref); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "tmux", "-L", "mergeyard", "attach-session", "-r", "-t", "="+ref.Name)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
		if err := cmd.Run(); err != nil {
			return &fault.Error{Code: "phase.watch_failed", Path: ref.Name, Message: "Could not attach read-only to the phase session", Err: err}
		}
		return nil
	}
	return nil
}

func printHarnessWait(out io.Writer, v workflow.HarnessWait) {
	fmt.Fprintf(out, "  Usage-limit wait: %s · %s · observed reset %s · %s · %s", terminalText(v.Harness), terminalText(v.Reason), v.ResetAt.Format(time.RFC3339), terminalText(v.ResetTimeSource), terminalText(v.Source))
	if v.AttemptID == "" {
		fmt.Fprintln(out, " · Account gate; no execution started.")
		return
	}
	fmt.Fprintf(out, " · interrupted execution %s · consecutive %d / allowance %d\n", terminalText(v.AttemptID), v.Consecutive, v.Allowance)
}

func printHandback(out io.Writer, v workflow.HandbackSnapshot) {
	if v.Pending {
		fmt.Fprintln(out, "  Handback publication and reconciliation pending")
	}
	if v.Error != "" {
		fmt.Fprintf(out, "  Handback requires attention: %s\n", terminalText(v.Error))
	}
	if v.NextPhase != "" {
		fmt.Fprintf(out, "  Handback selected %s/%s · round %d\n", terminalText(string(v.NextState)), terminalText(string(v.NextPhase)), v.Round)
	}
	if v.GrantedRound > 0 {
		if !v.Pending && v.Error == "" {
			fmt.Fprintf(out, "  Additional review round granted: %d\n", v.GrantedRound)
		} else {
			fmt.Fprintf(out, "  Additional review round requested: %d\n", v.GrantedRound)
		}
	}
}

func printRetry(out io.Writer, v workflow.RetrySnapshot) {
	if v.Pending {
		fmt.Fprintln(out, "  Retry requested; reconciliation pending")
		return
	}
	if v.Error != "" {
		fmt.Fprintf(out, "  Retry requires attention: %s\n", terminalText(v.Error))
		return
	}
	fmt.Fprintf(out, "  Retry selected %s/%s · round %d\n", terminalText(string(v.NextState)), terminalText(string(v.NextPhase)), v.Round)
	if v.GrantedWait > 0 {
		fmt.Fprintf(out, "  Exactly one additional usage-limit wait granted; allowance %d. Known reset time remains in effect.\n", v.GrantedWait)
	}
	if v.GrantedRound > 0 {
		fmt.Fprintf(out, "  Additional review round granted: %d\n", v.GrantedRound)
	}
	if v.Deadline != "" {
		fmt.Fprintf(out, "  Renewed CI deadline: %s\n", terminalText(v.Deadline))
	}
}

// Escape terminal controls at the presentation boundary. Keep report text intact
// in persistence and leave printable Unicode readable in command output.
func terminalText(value string) string {
	var text strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			quoted := strconv.QuoteRuneToASCII(r)
			text.WriteString(quoted[1 : len(quoted)-1])
		} else {
			text.WriteRune(r)
		}
	}
	return text.String()
}
