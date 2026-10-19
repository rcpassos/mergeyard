package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rcpassos/mergeyard/internal/web"
)

const (
	dashboardLinksDirName  = ".dashboard-links"
	dashboardHandoffPrefix = "handoff-"
	dashboardHandoffFile   = "dashboard.html"
)

// openBrowser opens a single-use dashboard link in the platform browser
// without exposing its token in a process command line. Other local users can
// read command arguments, so the URL goes into an owner-only handoff page
// inside the workspace and only that path reaches the browser launcher.
func openBrowser(ctx context.Context, root, address string) error {
	cleanupDashboardHandoffs(root)
	page, err := writeDashboardHandoffPage(root, address)
	if err != nil {
		return err
	}
	executable := "xdg-open"
	if runtime.GOOS == "darwin" {
		executable = "open"
	}
	cmd := exec.CommandContext(ctx, executable, page)
	if err := cmd.Start(); err != nil {
		// The launcher never started, so it never read the page.
		_ = os.RemoveAll(filepath.Dir(page))
		return err
	}
	return cmd.Wait()
}

// writeDashboardHandoffPage writes an owner-only page that redirects the
// browser to the single-use link. The private directory keeps every page out of
// the rest of the workspace, and the page is set to 0600 after the write in
// case an unusually strict umask removed the owner's read bit.
func writeDashboardHandoffPage(root, address string) (string, error) {
	links, err := dashboardLinksDir(root)
	if err != nil {
		return "", fmt.Errorf("prepare private dashboard handoff: %w", err)
	}
	dir, err := os.MkdirTemp(links, dashboardHandoffPrefix)
	if err != nil {
		return "", fmt.Errorf("prepare private dashboard handoff: %w", err)
	}
	path := filepath.Join(dir, dashboardHandoffFile)
	target := html.EscapeString(address)
	page := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0; url=%s">
<title>Opening Mergeyard</title>
</head>
<body>
<p>Opening the Mergeyard dashboard…</p>
<p><a href="%s">Continue to Mergeyard</a></p>
</body>
</html>
`, target, target)
	if err := os.WriteFile(path, []byte(page), 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("write private dashboard handoff: %w", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("secure private dashboard handoff: %w", err)
	}
	return path, nil
}

// cleanupDashboardHandoffs removes handoff pages whose token has expired. The
// pages are owner-only and contain no usable token after the lifetime, but
// leaving them would accumulate one directory per open. Only the dedicated
// handoff directory is touched.
func cleanupDashboardHandoffs(root string) {
	handoffRoot, err := dashboardLinksDir(root)
	if err != nil {
		return
	}
	entries, err := os.ReadDir(handoffRoot)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-web.DashboardTokenLifetime)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), dashboardHandoffPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(handoffRoot, entry.Name()))
	}
}

// dashboardLinksDir returns the private directory that holds handoff pages.
func dashboardLinksDir(root string) (string, error) {
	dir := filepath.Join(root, dashboardLinksDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}
