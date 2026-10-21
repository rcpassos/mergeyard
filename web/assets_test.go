package webassets

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The dashboard uses stock daisyUI light and dark themes: the stylesheet
// source picks them and never sets its own colors or fonts.
func TestStylesheetUsesStockThemesAndSystemFonts(t *testing.T) {
	source, err := os.ReadFile("src/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(source)
	if !regexp.MustCompile(`themes:\s*light --default,\s*dark --prefersdark;`).MatchString(css) {
		t.Errorf("stylesheet does not select the stock light and dark themes:\n%s", css)
	}
	for name, pattern := range map[string]string{
		"hex color":              `#[0-9a-fA-F]{3,8}\b`,
		"color function":         `\b(rgba?|hsla?|oklch|oklab|lab|lch|color)\(`,
		"daisyUI color override": `--color-[\w-]+\s*:`,
		"font family":            `font-family\s*:`,
		"font file":              `@font-face`,
	} {
		if match := regexp.MustCompile(pattern).FindString(css); match != "" {
			t.Errorf("stylesheet has a custom %s: %q", name, match)
		}
	}

	built, err := Files.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(built), "prefers-color-scheme:dark") {
		t.Error("built stylesheet has no dark theme for the system preference; rebuild the assets")
	}
	// outcomeAlert picks variants in Go, where Tailwind does not look.
	for _, variant := range []string{"alert-info", "alert-success", "alert-warning", "alert-error"} {
		if !strings.Contains(string(built), "."+variant+"{") {
			t.Errorf("built stylesheet lacks .%s", variant)
		}
	}
	if !strings.Contains(css, `@source inline("alert-info alert-success alert-warning alert-error")`) {
		t.Error("stylesheet does not keep the alert variants outcomeAlert returns from Go")
	}
	// statusBadge picks badge variants in Go too.
	for _, variant := range []string{"badge-neutral", "badge-info", "badge-accent", "badge-warning", "badge-error", "badge-success", "badge-outline"} {
		if !strings.Contains(string(built), "."+variant+"{") {
			t.Errorf("built stylesheet lacks .%s", variant)
		}
	}
}
