package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcpassos/mergeyard/internal/web"
)

func assertHandoffPage(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("handoff page is not owner-only: %v %v", info, err)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("handoff directory is not private: %v %v", dir, err)
	}
}

func useBrowserPlatform(t *testing.T, platform string) {
	t.Helper()
	previous := browserPlatform
	browserPlatform = platform
	t.Cleanup(func() { browserPlatform = previous })
}

func writeTool(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupDashboardHandoffsRemovesOnlyExpiredPages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	links, err := dashboardLinksDir(t.TempDir(), "linux")
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) string {
		t.Helper()
		dir := filepath.Join(links, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dashboardHandoffFile), []byte("expired token"), 0600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	expired := create(dashboardHandoffPrefix + "expired")
	recent := create(dashboardHandoffPrefix + "recent")
	unrelated := create("notes")
	tampered := create(dashboardHandoffPrefix + "tampered")
	if err := os.WriteFile(filepath.Join(tampered, "keep.txt"), []byte("user data"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-web.DashboardTokenLifetime - time.Minute)
	for _, dir := range []string{expired, tampered} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}

	cleanupDashboardHandoffs(t.TempDir(), "linux")

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired handoff page survived cleanup: %v", err)
	}
	for _, kept := range []string{recent, unrelated, tampered} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("cleanup removed %s: %v", kept, err)
		}
	}
}

func TestOpenBrowserUsesDefaultWebBrowserAndSnapAccessiblePage(t *testing.T) {
	useBrowserPlatform(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	tools := t.TempDir()
	launched, htmlHandler := filepath.Join(tools, "launched"), filepath.Join(tools, "html-handler")
	writeTool(t, tools, "xdg-settings", "#!/bin/sh\necho browser.desktop\n")
	writeTool(t, tools, "gio", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+launched+"'\n")
	writeTool(t, tools, "xdg-open", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+htmlHandler+"'\n")
	t.Setenv("PATH", tools)
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_DATA_DIRS", "")
	desktop := filepath.Join(dataHome, "applications", "browser.desktop")
	if err := os.MkdirAll(filepath.Dir(desktop), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktop, []byte("[Desktop Entry]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	address := "http://127.0.0.1:7331/?token=one-time"

	if err := openBrowser(context.Background(), t.TempDir(), address); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(launched)
	if err != nil {
		t.Fatalf("default browser was not launched: %v", err)
	}
	if strings.Contains(string(args), "one-time") || strings.Contains(string(args), "token=") {
		t.Fatalf("launcher received the dashboard token: %q", args)
	}
	fields := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(fields) != 3 || fields[0] != "launch" || fields[1] != desktop {
		t.Fatalf("launcher did not select the default web browser: %q", args)
	}
	page := fields[2]
	assertHandoffPage(t, page)
	relative, err := filepath.Rel(home, page)
	if err != nil || strings.HasPrefix(relative, ".") || !strings.HasPrefix(relative, linuxDashboardLinksDirName+string(filepath.Separator)) {
		t.Fatalf("handoff page is not in a Snap-accessible home location: %q %v", relative, err)
	}
	linksDir, err := os.Stat(filepath.Join(home, linuxDashboardLinksDirName))
	if err != nil || linksDir.Mode().Perm() != 0700 {
		t.Fatalf("handoff root is not owner-only: %v %v", linksDir, err)
	}
	content, err := os.ReadFile(page)
	if err != nil || !strings.Contains(string(content), address) {
		t.Fatalf("handoff page does not redirect to the link: %q %v", content, err)
	}
	if _, err := os.Stat(htmlHandler); !os.IsNotExist(err) {
		t.Fatalf("html file association was used instead of the web browser: %v", err)
	}
}

func TestOpenBrowserFallbackLaunchesTheResolvedBrowserCommand(t *testing.T) {
	useBrowserPlatform(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	tools := t.TempDir()
	launched, htmlHandler := filepath.Join(tools, "launched"), filepath.Join(tools, "html-handler")
	browser := filepath.Join(tools, "review-browser")
	writeTool(t, tools, "review-browser", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+launched+"'\n")
	writeTool(t, tools, "xdg-settings", "#!/bin/sh\necho review-browser.desktop\n")
	writeTool(t, tools, "xdg-open", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+htmlHandler+"'\n")
	t.Setenv("PATH", tools)
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_DATA_DIRS", "")
	desktop := filepath.Join(dataHome, "applications", "review-browser.desktop")
	if err := os.MkdirAll(filepath.Dir(desktop), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktop, []byte("[Desktop Entry]\nExec="+browser+" %u\n"), 0600); err != nil {
		t.Fatal(err)
	}
	address := "http://127.0.0.1:7331/?token=one-time"

	if err := openBrowser(context.Background(), t.TempDir(), address); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(launched)
	if err != nil {
		t.Fatalf("resolved browser was not launched: %v", err)
	}
	if strings.Contains(string(args), "one-time") || strings.Contains(string(args), "token=") {
		t.Fatalf("browser command received the dashboard token: %q", args)
	}
	if !strings.Contains(string(args), "file://") || !strings.Contains(string(args), dashboardHandoffFile) {
		t.Fatalf("browser command did not receive the handoff page: %q", args)
	}
	links, err := filepath.Glob(filepath.Join(home, linuxDashboardLinksDirName, dashboardHandoffPrefix+"*", dashboardHandoffFile))
	if err != nil || len(links) != 1 {
		t.Fatalf("handoff page: %q %v", links, err)
	}
	assertHandoffPage(t, links[0])
	page, err := os.ReadFile(links[0])
	if err != nil || !strings.Contains(string(page), address) {
		t.Fatalf("handoff page does not redirect to the link: %q %v", page, err)
	}
	if _, err := os.Stat(htmlHandler); !os.IsNotExist(err) {
		t.Fatalf("html file association was used instead of the web browser: %v", err)
	}
}

func TestOpenBrowserReportsMissingBrowserWithoutUsingTheHTMLHandler(t *testing.T) {
	useBrowserPlatform(t, "linux")
	home := t.TempDir()
	t.Setenv("HOME", home)
	tools := t.TempDir()
	htmlHandler := filepath.Join(tools, "html-handler")
	writeTool(t, tools, "xdg-settings", "#!/bin/sh\necho missing-browser.desktop\n")
	writeTool(t, tools, "xdg-open", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+htmlHandler+"'\n")
	t.Setenv("PATH", tools)
	emptyData := t.TempDir()
	t.Setenv("XDG_DATA_HOME", emptyData)
	t.Setenv("XDG_DATA_DIRS", emptyData)

	if err := openBrowser(context.Background(), t.TempDir(), "http://127.0.0.1:7331/?token=one-time"); err == nil {
		t.Fatal("openBrowser accepted a browser that could not be selected")
	} else if !strings.Contains(err.Error(), "HTTP browser launcher") {
		t.Fatalf("missing browser guidance: %v", err)
	}
	if _, err := os.Stat(htmlHandler); !os.IsNotExist(err) {
		t.Fatalf("html file association was used instead of reporting the missing browser: %v", err)
	}
}

func TestOpenMacBrowserKeepsTokenOffTheCommandLine(t *testing.T) {
	useBrowserPlatform(t, "darwin")
	tools := t.TempDir()
	argsPath, stdinPath := filepath.Join(tools, "args"), filepath.Join(tools, "stdin")
	writeTool(t, tools, "osascript", "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+argsPath+"'\n/bin/cat > '"+stdinPath+"'\n")
	t.Setenv("PATH", tools)
	address := "http://127.0.0.1:7331/?token=one-time"

	if err := openBrowser(context.Background(), t.TempDir(), address); err != nil {
		t.Fatal(err)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil || strings.Contains(string(args), "one-time") || strings.Contains(string(args), "token=") {
		t.Fatalf("osascript arguments carried the token: %q %v", args, err)
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil || !strings.Contains(string(stdin), `open location "`+address+`"`) {
		t.Fatalf("osascript did not open the default browser: %q %v", stdin, err)
	}
}
