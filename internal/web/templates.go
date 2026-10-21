package web

import (
	"html/template"

	webassets "github.com/rcpassos/mergeyard/web"
)

// templateFiles lists the dashboard template groups: the layout, one file
// per page, and the partials pages share.
var templateFiles = []string{"templates/*.html", "templates/pages/*.html", "templates/partials/*.html"}

// templateHelpers is the function registry available to every dashboard
// template. Presentation helpers belong here, not in the page data.
func templateHelpers() template.FuncMap {
	return template.FuncMap{}
}

// parseTemplates builds the dashboard's template set from the embedded files.
func parseTemplates() (*template.Template, error) {
	return template.New("dashboard").Funcs(templateHelpers()).ParseFS(webassets.Files, templateFiles...)
}
