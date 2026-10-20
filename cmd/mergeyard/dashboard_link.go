package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/rcpassos/mergeyard/internal/web"
)

const (
	dashboardLinksDirName      = ".dashboard-links"
	linuxDashboardLinksDirName = "mergeyard-dashboard-handoff"
	dashboardHandoffPrefix     = "handoff-"
	dashboardHandoffFile       = "dashboard.html"
	// missingBrowserLauncher is reported when no launcher could start.
	missingBrowserLauncher = "could not find an HTTP browser launcher; install gio or open the printed dashboard link"
)

// browserPlatform is runtime.GOOS. Tests set it to exercise each launcher
// branch without depending on the host.
var browserPlatform = runtime.GOOS

// openBrowser opens a single-use dashboard link in the platform browser
// without exposing its token in a process command line. On macOS, AppleScript
// asks LaunchServices for the default HTTP browser and receives the URL over
// stdin. Elsewhere, the URL goes into an owner-only handoff page and only that
// path reaches the explicitly selected browser. The launchers that could not
// start are reported to stderr.
func openBrowser(ctx context.Context, root, address string, stderr io.Writer) error {
	if browserPlatform == "darwin" {
		return openMacBrowser(ctx, address)
	}
	cleanupDashboardHandoffs(root, browserPlatform)
	page, err := writeDashboardHandoffPage(root, address, browserPlatform)
	if err != nil {
		return err
	}
	return launchHandoffPage(ctx, page, stderr)
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
// with an editor while keeping a separate HTTP browser. If neither gio nor
// gtk-launch is installed, the browser command is read from the selected
// desktop entry. The page is never handed to xdg-open, which would use the
// HTML association.
//
// The browser is never waited for. A launcher that cannot start is reported
// and the next fallback is tried; when no launcher starts, every failure is
// part of the returned error.
func launchHandoffPage(ctx context.Context, page string, stderr io.Writer) error {
	failures, launched := tryHandoffLaunchers(ctx, page)
	if launched {
		for _, failure := range failures {
			fmt.Fprintf(stderr, "mergeyard: browser launcher not used: %v\n", failure)
		}
		return nil
	}
	if len(failures) == 0 {
		return errors.New(missingBrowserLauncher)
	}
	return fmt.Errorf("%s: %w", missingBrowserLauncher, errors.Join(failures...))
}

// tryHandoffLaunchers starts the launchers in order and reports the first one
// that started. The failures are the launchers that could not start, so the
// caller can report them.
func tryHandoffLaunchers(ctx context.Context, page string) (failures []error, launched bool) {
	launch := func(name string, args ...string) bool {
		if err := startDetachedLauncher(name, args...); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			return false
		}
		return true
	}
	if id := defaultBrowserID(ctx); id != "" {
		if desktop, ok := desktopFilePath(id); ok {
			if launch("gio", "launch", desktop, page) {
				return failures, true
			}
			if launch("gtk-launch", id, page) {
				return failures, true
			}
			args, err := desktopBrowserArgs(desktop, page)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", desktop, err))
			} else if launch(args[0], args[1:]...) {
				return failures, true
			}
		} else if launch("gtk-launch", id, page) {
			return failures, true
		}
	}
	for _, name := range []string{"sensible-browser", "x-www-browser"} {
		if launch(name, page) {
			return failures, true
		}
	}
	return failures, false
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

// desktopBrowserArgs reads the selected desktop entry's Exec command and
// expands its file field codes into the arguments that open the handoff page.
// It preserves the HTTP browser choice when gio and gtk-launch are not
// installed.
func desktopBrowserArgs(desktopPath, page string) ([]string, error) {
	command, err := desktopExec(desktopPath)
	if err != nil {
		return nil, err
	}
	args, ok := desktopLaunchArgs(command, page, desktopPath)
	if !ok {
		return nil, errors.New("desktop entry has no executable command")
	}
	return args, nil
}

func desktopExec(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	inEntry := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inEntry = line == "[Desktop Entry]"
			continue
		}
		if !inEntry || !strings.HasPrefix(line, "Exec=") {
			continue
		}
		command, err := splitDesktopCommand(strings.TrimPrefix(line, "Exec="))
		if err != nil {
			return nil, err
		}
		if len(command) == 0 {
			return nil, fmt.Errorf("desktop entry has an empty Exec command")
		}
		return command, nil
	}
	return nil, fmt.Errorf("desktop entry has no Exec command")
}

// splitDesktopCommand parses the quoting and escapes used by a desktop Exec
// value. Single quotes are accepted because some desktop files use them even
// though the specification names only double quotes.
func splitDesktopCommand(value string) ([]string, error) {
	var fields []string
	var current strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() {
		if started {
			fields = append(fields, current.String())
			current.Reset()
			started = false
		}
	}
	for _, r := range value {
		if escaped {
			switch r {
			case 's':
				current.WriteByte(' ')
			case 'n':
				current.WriteByte('\n')
			case 't':
				current.WriteByte('\t')
			case 'r':
				current.WriteByte('\r')
			default:
				current.WriteRune(r)
			}
			escaped, started = false, true
			continue
		}
		switch {
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			started = true
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t':
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped {
		current.WriteByte('\\')
	}
	if quote != 0 {
		return nil, fmt.Errorf("desktop entry has an unterminated quote")
	}
	flush()
	return fields, nil
}

func desktopLaunchArgs(command []string, page, desktopPath string) ([]string, bool) {
	fileURL := (&url.URL{Scheme: "file", Path: page}).String()
	args := make([]string, 0, len(command)+1)
	placed := false
	for _, field := range command {
		if field == "%i" {
			continue
		}
		expanded, hasFile := expandDesktopFieldCodes(field, page, fileURL, desktopPath)
		if expanded == "" {
			continue
		}
		args = append(args, expanded)
		placed = placed || hasFile
	}
	if len(args) == 0 {
		return nil, false
	}
	if !placed {
		args = append(args, fileURL)
	}
	return args, true
}

func expandDesktopFieldCodes(value, page, fileURL, desktopPath string) (string, bool) {
	var expanded strings.Builder
	placed := false
	for i := 0; i < len(value); i++ {
		if value[i] != '%' || i+1 >= len(value) {
			expanded.WriteByte(value[i])
			continue
		}
		switch value[i+1] {
		case 'f', 'F':
			expanded.WriteString(page)
			placed = true
		case 'u', 'U':
			expanded.WriteString(fileURL)
			placed = true
		case 'k':
			expanded.WriteString(desktopPath)
		case '%':
			expanded.WriteByte('%')
		case 'c', 'i':
			// Application name and icon expansion are not needed here.
		default:
			// Unknown field codes are removed rather than passed literally.
		}
		i++
	}
	return expanded.String(), placed
}

// startDetachedLauncher starts a browser launcher in its own session and
// process group. The launcher is not tied to Mergeyard's context: the command
// never waits for it, and stopping Mergeyard never terminates it. The browser
// therefore outlives the command that opened it. A start error means no
// process ran, so the caller can report it and try the next launcher.
func startDetachedLauncher(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the launcher once it exits so a closed browser does not linger as a
	// zombie. The browser it started keeps running in its own session.
	go func() { _ = cmd.Wait() }()
	return nil
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
