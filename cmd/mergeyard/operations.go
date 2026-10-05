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
	"github.com/rcpassos/mergeyard/internal/runner"
	"github.com/rcpassos/mergeyard/internal/web"
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
		for _, run := range status.Runs {
			if run.State.Terminal() {
				continue
			}
			count++
			fmt.Fprintf(stdout, "%s  %s#%d  %s/%s\n", terminalText(run.ID), terminalText(run.Repository), run.IssueNumber, terminalText(string(run.State)), terminalText(string(run.Phase)))
			if run.LastErrorCode != "" {
				fmt.Fprintf(stdout, "  %s: %s\n", terminalText(run.LastErrorCode), terminalText(run.LastErrorMessage))
			}
			if v := run.Implementer; v != nil {
				fmt.Fprintf(stdout, "  implementer %s · model %s · effort %s · skills %s · permissions %s · session %s\n", terminalText(v.Agent), terminalText(v.Model), terminalText(v.Effort), terminalText(strings.Join(v.Skills, ",")), terminalText(v.Permissions), terminalText(v.SessionID))
				fmt.Fprintf(stdout, "  implement attempt %d: %s · process %s\n", v.Attempt, terminalText(v.Status), terminalText(v.ProcessSession))
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
