package webassets

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The public website shares the dashboard's stock daisyUI themes through a
// stylesheet the asset build writes into site/.
func TestWebsiteUsesGeneratedTheme(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	theme := read("../site/theme.css")
	lightPrimary := regexp.MustCompile(`(?m)^:root\{[^}]*--color-primary:([^;]+);`).FindStringSubmatch(theme)
	if lightPrimary == nil {
		t.Fatalf("site/theme.css does not set the light primary on :root; rebuild the assets:\n%s", theme)
	}
	if !strings.Contains(theme, "@media (prefers-color-scheme: dark){:root{") {
		t.Errorf("site/theme.css has no dark theme for the system preference; rebuild the assets:\n%s", theme)
	}

	page := read("../site/index.html")
	themeLink := strings.Index(page, `<link rel="stylesheet" href="./theme.css">`)
	styleLink := strings.Index(page, `<link rel="stylesheet" href="./style.css">`)
	if themeLink < 0 || styleLink < themeLink {
		t.Error("site/index.html does not load theme.css before style.css")
	}
	if match := regexp.MustCompile(`(?:href|src)="(?:https?:)?//[^"]+\.(?:css|js|woff2?|ttf|otf)"`).FindString(page); match != "" {
		t.Errorf("site/index.html loads an external asset: %s", match)
	}

	style := read("../site/style.css")
	for name, pattern := range map[string]string{
		"hex color":              `#[0-9a-fA-F]{3,8}\b`,
		"color function":         `\b(rgba?|hsla?|oklch|oklab|lab|lch|color)\(`,
		"named color":            `:\s*(?:white|black)\b`,
		"daisyUI color override": `--color-[\w-]+\s*:`,
		"palette variable":       `--(?:ink|blue|paper|white|muted|green|line)\b`,
		"font file":              `@font-face`,
	} {
		if match := regexp.MustCompile(pattern).FindString(style); match != "" {
			t.Errorf("site/style.css has a custom %s: %q", name, match)
		}
	}
	for _, stack := range []string{"font-family: ui-sans-serif, system-ui,", "font-family: ui-monospace,"} {
		if !strings.Contains(style, stack) {
			t.Errorf("site/style.css does not use the system font stack %q", stack)
		}
	}

	if read("../site/favicon.svg") != read("static/favicon.svg") {
		t.Error("site/favicon.svg differs from the dashboard favicon; rebuild the assets")
	}

	preview := read("../site/social-preview.svg")
	if !strings.Contains(preview, lightPrimary[1]) {
		t.Errorf("site/social-preview.svg is not drawn in the light primary %s", lightPrimary[1])
	}
}
