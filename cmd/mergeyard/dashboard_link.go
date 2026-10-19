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
	dashboardLinksDirName      = ".dashboard-links"
	linuxDashboardLinksDirName = "mergeyard-dashboard-handoff"
	dashboardHandoffPrefix     = "handoff-"
	dashboardHandoffFile       = "dashboard.html"
)

// browserPlatform is runtime.GOOS. Tests set it to exercise each launcher
// branch without depending on the host.
var browserPlatform = runtime.GOOS

// openBrowser opens a single-use dashboard link in the platform browser
// without exposing its token in a process command line. On macOS, AppleScript
// asks LaunchServices for the default HTTP browser and receives the URL over
// stdin. Elsewhere, the URL goes into an owner-only handoff page and only that
// path reaches the explicitly selected browser.
func openBrowser(ctx context.Context, root, address string) error {
	if browserPlatform == "darwin" {
		return openMacBrowser(ctx, address)
	}
	cleanupDashboardHandoffs(root, browserPlatform)
	page, err := writeDashboardHandoffPage(root, address, browserPlatform)
	if err != nil {
		return err
	}
	return launchHandoffPage(ctx, browserPlatform, page)
}

// openMacBrowser uses the HTTP handler rather than the HTML file association.
// The script travels over stdin, so neither the script nor the tokenized URL
// appears in a process command line.
func openMacBrowser(ctx context.Context, address string) error {
	cmd := exec.CommandContext(ctx, "osascript", "-")
	cmd.Stdin = strings.NewReader("open location " + appleScriptString(address) + "\n")
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Wait()
}

func appleScriptString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// launchHandoffPage opens the page with the default web browser, not the
// handler for text/html. That matters because a user may associate HTML files
// with an editor while keeping a separate HTTP browser.
func launchHandoffPage(ctx context.Context, platform, page string) error {
	if platform == "linux" {
		if id := defaultBrowserID(ctx); id != "" {
			if desktop, ok := desktopFilePath(id); ok {
				if wait, err := startLauncher(ctx, "gio", "launch", desktop, page); err == nil {
					return wait()
				}
			}
			if wait, err := startLauncher(ctx, "gtk-launch", id, page); err == nil {
				return wait()
			}
		}
	}
	wait, err := startLauncher(ctx, "xdg-open", page)
	if err != nil {
		return err
	}
	return wait()
}

func defaultBrowserID(ctx context.Context) string {
	for _, query := range [][]string{
		{"xdg-settings", "get", "default-web-browser"},
		{"xdg-mime", "query", "default", "x-scheme-handler/http"},
	} {
		output, err := exec.CommandContext(ctx, query[0], query[1:]...).Output()
		if err != nil {
			continue
		}
		id := strings.TrimSpace(string(output))
		if strings.HasSuffix(id, ".desktop") && !strings.ContainsAny(id, `/\`) {
			return id
		}
	}
	return ""
}

func desktopFilePath(id string) (string, bool) {
	home, _ := os.UserHomeDir()
	var roots []string
	if dataHome := os.Getenv("XDG_DATA_HOME"); dataHome != "" {
		roots = append(roots, dataHome)
	} else if home != "" {
		roots = append(roots, filepath.Join(home, ".local", "share"))
	}
	if dataDirs := os.Getenv("XDG_DATA_DIRS"); dataDirs != "" {
		roots = append(roots, filepath.SplitList(dataDirs)...)
	} else {
		roots = append(roots, "/usr/local/share", "/usr/share")
	}
	roots = append(roots, "/var/lib/snapd/desktop", "/var/lib/flatpak/exports/share")
	if home != "" {
		roots = append(roots, filepath.Join(home, ".local", "share", "flatpak", "exports", "share"))
	}
	for _, root := range roots {
		path := filepath.Join(root, "applications", id)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, true
		}
	}
	return "", false
}

// startLauncher starts name and returns its Wait function. A start error means
// no process ran, so the caller can try another browser launcher.
func startLauncher(ctx context.Context, name string, args ...string) (func() error, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

// writeDashboardHandoffPage writes an owner-only page that redirects the
// browser to the single-use link. The page is set to 0600 after the write in
// case an unusually strict umask removed the owner's read bit.
func writeDashboardHandoffPage(root, address, platform string) (string, error) {
	links, err := dashboardLinksDir(root, platform)
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
// handoff directory is touched, and only directories containing the expected
// handoff file are removed.
func cleanupDashboardHandoffs(root, platform string) {
	handoffRoot, err := dashboardLinksDir(root, platform)
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
		path := filepath.Join(handoffRoot, entry.Name())
		if !isHandoffDir(path) {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

func isHandoffDir(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 1 || entries[0].Name() != dashboardHandoffFile {
		return false
	}
	info, err := entries[0].Info()
	return err == nil && info.Mode().IsRegular()
}

// dashboardLinksDir returns the private directory that holds handoff pages.
// Linux uses a non-hidden home directory so confined browsers such as the
// Firefox snap can read the page. Hidden home paths are blocked by the snap
// home interface.
func dashboardLinksDir(root, platform string) (string, error) {
	name := dashboardLinksDirName
	if platform == "linux" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root, name = home, linuxDashboardLinksDirName
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}
