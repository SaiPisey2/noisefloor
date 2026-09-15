package server

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// funcMap is deliberately small. Every function here formats a plain
// number or duration for display; none of them touch untrusted text, which
// keeps html/template's auto-escaping the only thing standing between a
// rule name or annotation and the page -- see the package doc comment on
// why template.HTML never appears in this package.
var funcMap = template.FuncMap{
	"pct":      pct,
	"duration": formatDuration,
	"pctOf":    pctOf,
}

func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html")
}
