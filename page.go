// Package savingspreview embeds the report page sources so the CLI binary can
// serve the page without any files next to it.
package savingspreview

import (
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

//go:embed src/index.html src/js/*.js src/styles/*.css src/fonts/*.woff2
var src embed.FS

// The lists and markers below mirror scripts/build_report.py, so both produce
// the same page.
var (
	scripts = []string{"model.js", "chart.js", "app.js"}
	styles  = []string{"zinc.css", "primitives.css", "semantic.css", "report.css"}
	fonts   = []struct {
		family string
		weight int
		file   string
	}{
		{"Roboto", 400, "roboto-latin-400-normal.woff2"},
		{"Roboto", 500, "roboto-latin-500-normal.woff2"},
		{"Roboto", 700, "roboto-latin-700-normal.woff2"},
		{"Roboto Mono", 400, "roboto-mono-latin-400-normal.woff2"},
		{"Roboto Mono", 500, "roboto-mono-latin-500-normal.woff2"},
	}
)

// ReportPage returns the report page as one HTML document with its scripts,
// styles and fonts inlined.
func ReportPage() (string, error) {
	read := func(name string) (string, error) {
		b, err := src.ReadFile("src/" + name)
		return string(b), err
	}
	var faces []string
	for _, f := range fonts {
		b, err := src.ReadFile("src/fonts/" + f.file)
		if err != nil {
			return "", err
		}
		faces = append(faces, fmt.Sprintf("@font-face{font-family:'%s';font-weight:%d;font-style:normal;font-display:swap;"+
			"src:url(data:font/woff2;base64,%s) format('woff2')}", f.family, f.weight, base64.StdEncoding.EncodeToString(b)))
	}
	var css, js []string
	for _, n := range styles {
		s, err := read("styles/" + n)
		if err != nil {
			return "", err
		}
		css = append(css, s)
	}
	for _, n := range scripts {
		s, err := read("js/" + n)
		if err != nil {
			return "", err
		}
		js = append(js, s)
	}
	script := strings.Join(js, "\n")
	if strings.Contains(strings.ToLower(script), "</script") {
		return "", errors.New("script text contains a closing script tag")
	}
	page, err := read("index.html")
	if err != nil {
		return "", err
	}
	style := strings.Replace(strings.Join(css, "\n"), "/*{{FONTS}}*/", strings.Join(faces, "\n"), -1)
	page = strings.Replace(page, "/*{{CSS}}*/", style, 1)
	return strings.Replace(page, "/*{{JS}}*/", script, 1), nil
}
