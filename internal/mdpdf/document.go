package mdpdf

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v2/styles"
)

//go:embed assets
var assetFS embed.FS

// Options control rendering. Every field is optional.
type Options struct {
	Theme       string  `json:"theme"`      // github | github-dark | academic | modern | none
	CodeTheme   string  `json:"code_theme"` // any chroma style, e.g. github, dracula, monokai
	PageSize    string  `json:"page_size"`  // A3 A4 A5 B5 Letter Legal Tabloid Executive
	Width       string  `json:"width"`      // custom page width  (e.g. "180mm"); overrides page_size
	Height      string  `json:"height"`     // custom page height
	Landscape   bool    `json:"landscape"`
	Margin      Margin  `json:"margin"` // "20mm" | {"top":"1in","right":...}
	Scale       float64 `json:"scale"`  // 0.1 – 2
	Background  *bool   `json:"print_background"`
	PageNumbers bool    `json:"page_numbers"`
	Header      string  `json:"header_html"` // Chrome header template (classes: pageNumber totalPages title date url)
	Footer      string  `json:"footer_html"`
	PageRanges  string  `json:"page_ranges"` // e.g. "1-3, 5"
	TOC         bool    `json:"toc"`
	LineNumbers bool    `json:"line_numbers"`
	SmartQuotes bool    `json:"smart_quotes"`
	HardBreaks  bool    `json:"hard_breaks"`
	FontFamily  string  `json:"font_family"`
	FontSize    string  `json:"font_size"`
	CSS         string  `json:"css"` // appended last, so it overrides everything
	Title       string  `json:"title"`
	Lang        string  `json:"lang"`
}

type Margin struct{ Top, Right, Bottom, Left string }

func (m *Margin) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		// CSS shorthand: 1–4 values.
		p := strings.Fields(s)
		switch len(p) {
		case 0:
		case 1:
			*m = Margin{p[0], p[0], p[0], p[0]}
		case 2:
			*m = Margin{p[0], p[1], p[0], p[1]}
		case 3:
			*m = Margin{p[0], p[1], p[2], p[1]}
		default:
			*m = Margin{p[0], p[1], p[2], p[3]}
		}
		return nil
	}
	var o struct{ Top, Right, Bottom, Left string }
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("margin must be a CSS length string or {top,right,bottom,left}")
	}
	*m = Margin(o)
	return nil
}

var themes = map[string]bool{"github": true, "github-dark": true, "academic": true, "modern": true, "none": true}

// pageSizes in inches (width, height), portrait.
var pageSizes = map[string][2]float64{
	"a3": {11.69, 16.54}, "a4": {8.27, 11.69}, "a5": {5.83, 8.27}, "b5": {6.93, 9.84},
	"letter": {8.5, 11}, "legal": {8.5, 14}, "tabloid": {11, 17}, "executive": {7.25, 10.5},
}

func Themes() []string {
	out := make([]string, 0, len(themes))
	for k := range themes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func CodeThemes() []string { return styles.Names() }

func PageSizes() []string {
	return []string{"A3", "A4", "A5", "B5", "Letter", "Legal", "Tabloid", "Executive"}
}

// cssLength parses a CSS absolute length into inches.
var lengthRe = regexp.MustCompile(`^\s*([0-9]*\.?[0-9]+)\s*(mm|cm|in|px|pt|pc)?\s*$`)

func cssLength(s string) (float64, bool) {
	m := lengthRe.FindStringSubmatch(strings.ToLower(s))
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "mm", "":
		v /= 25.4
	case "cm":
		v /= 2.54
	case "px":
		v /= 96
	case "pt":
		v /= 72
	case "pc":
		v /= 6
	}
	return v, true
}

// printSettings is the validated, Chrome-ready form of Options.
type printSettings struct {
	width, height                       float64
	marginT, marginR, marginB, marginL  float64
	landscape, background, headerFooter bool
	scale                               float64
	header, footer, pageRanges          string
}

const maxTemplateBytes = 8 << 10

func (o *Options) normalize() (printSettings, error) {
	ps := printSettings{scale: 1, background: true, landscape: o.Landscape}
	if o.Theme == "" {
		o.Theme = "github"
	}
	if !themes[o.Theme] {
		return ps, fmt.Errorf("unknown theme %q (available: %s)", o.Theme, strings.Join(Themes(), ", "))
	}
	if o.CodeTheme == "" {
		o.CodeTheme = "github"
		if o.Theme == "github-dark" {
			o.CodeTheme = "github-dark"
		}
	}
	if s := styles.Get(o.CodeTheme); s == nil || (s == styles.Fallback && o.CodeTheme != "swapoff") {
		return ps, fmt.Errorf("unknown code_theme %q", o.CodeTheme)
	}

	size := strings.ToLower(o.PageSize)
	if size == "" {
		size = "a4"
	}
	dim, ok := pageSizes[size]
	if !ok {
		return ps, fmt.Errorf("unknown page_size %q (available: %s)", o.PageSize, strings.Join(PageSizes(), ", "))
	}
	ps.width, ps.height = dim[0], dim[1]
	if o.Width != "" || o.Height != "" {
		w, ok1 := cssLength(o.Width)
		h, ok2 := cssLength(o.Height)
		if !ok1 || !ok2 || w < 1 || h < 1 || w > 100 || h > 200 {
			return ps, fmt.Errorf("width and height must both be lengths between 1in and 100in/200in (e.g. \"180mm\")")
		}
		ps.width, ps.height = w, h
	}

	def := "18mm"
	m := o.Margin
	for _, f := range []struct {
		src *string
		dst *float64
	}{{&m.Top, &ps.marginT}, {&m.Right, &ps.marginR}, {&m.Bottom, &ps.marginB}, {&m.Left, &ps.marginL}} {
		if *f.src == "" {
			*f.src = def
		}
		v, ok := cssLength(*f.src)
		if !ok || v > 4 {
			return ps, fmt.Errorf("invalid margin %q", *f.src)
		}
		*f.dst = v
	}

	if o.Scale != 0 {
		if o.Scale < 0.1 || o.Scale > 2 {
			return ps, fmt.Errorf("scale must be between 0.1 and 2")
		}
		ps.scale = o.Scale
	}
	if o.Background != nil {
		ps.background = *o.Background
	}
	if len(o.Header) > maxTemplateBytes || len(o.Footer) > maxTemplateBytes {
		return ps, fmt.Errorf("header/footer templates are limited to %d bytes", maxTemplateBytes)
	}
	ps.header, ps.footer = o.Header, o.Footer
	if o.PageNumbers && ps.footer == "" {
		ps.footer = `<div style="width:100%;font-size:8px;color:#888;text-align:center;font-family:system-ui,sans-serif"><span class="pageNumber"></span> / <span class="totalPages"></span></div>`
	}
	if ps.header != "" || ps.footer != "" {
		ps.headerFooter = true
		if ps.header == "" {
			ps.header = "<span></span>"
		}
		if ps.footer == "" {
			ps.footer = "<span></span>"
		}
		// Templates render inside the margin; make sure there is room.
		ps.marginT = max(ps.marginT, 0.45)
		ps.marginB = max(ps.marginB, 0.45)
	}
	if o.PageRanges != "" && !regexp.MustCompile(`^[0-9,\-\s]+$`).MatchString(o.PageRanges) {
		return ps, fmt.Errorf("page_ranges must look like \"1-3, 5\"")
	}
	ps.pageRanges = o.PageRanges
	if o.Lang != "" && !regexp.MustCompile(`^[a-zA-Z]{2,3}(-[a-zA-Z0-9]{2,8})*$`).MatchString(o.Lang) {
		return ps, fmt.Errorf("invalid lang %q", o.Lang)
	}
	return ps, nil
}

// document is a fully built print page plus the CSP that guards it.
type document struct {
	HTML  []byte
	CSP   string
	Title string
}

var (
	fontFamilyRe = regexp.MustCompile(`^[\w\s,"'.-]{1,200}$`)
	fontSizeRe   = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,2})?(px|pt|em|rem|%)$`)
)

func buildDocument(markdown string, o *Options) (*document, error) {
	r, err := renderMarkdown(markdown, mdOptions{
		CodeTheme: o.CodeTheme, LineNumbers: o.LineNumbers, TOC: o.TOC,
		SmartQuotes: o.SmartQuotes, HardBreaks: o.HardBreaks,
	})
	if err != nil {
		return nil, err
	}
	title := o.Title
	if title == "" {
		title = r.Title
	}
	if title == "" {
		title = "Document"
	}
	lang := o.Lang
	if lang == "" {
		lang = "en"
	}

	nb := make([]byte, 18)
	_, _ = rand.Read(nb)
	nonce := base64.RawURLEncoding.EncodeToString(nb)

	var b strings.Builder
	b.Grow(len(r.HTML) + 64<<10)
	fmt.Fprintf(&b, "<!DOCTYPE html>\n<html lang=\"%s\">\n<head>\n<meta charset=\"utf-8\">\n", html.EscapeString(lang))
	fmt.Fprintf(&b, "<title>%s</title>\n", html.EscapeString(title))
	// Our scripts are declared in <head>, before any user markup, so user HTML
	// cannot use dangling markup to capture the nonce.
	if r.HasMath {
		b.WriteString("<link rel=\"stylesheet\" href=\"/assets/katex/katex.min.css\">\n")
		fmt.Fprintf(&b, "<script defer nonce=\"%s\" src=\"/assets/katex/katex.min.js\"></script>\n", nonce)
	}
	if r.HasMermaid {
		fmt.Fprintf(&b, "<script defer nonce=\"%s\" src=\"/assets/vendor/mermaid.min.js\"></script>\n", nonce)
	}
	fmt.Fprintf(&b, "<script defer nonce=\"%s\" src=\"/assets/page.js\"></script>\n", nonce)

	b.WriteString("<style>\n")
	b.WriteString(mustAsset("assets/themes/base.css"))
	if o.Theme != "none" {
		b.WriteString(mustAsset("assets/themes/" + o.Theme + ".css"))
	}
	b.WriteString(r.CodeCSS)
	var vars []string
	if o.FontFamily != "" {
		if !fontFamilyRe.MatchString(o.FontFamily) {
			return nil, fmt.Errorf("invalid font_family")
		}
		vars = append(vars, "body{font-family:"+o.FontFamily+"}")
	}
	if o.FontSize != "" {
		if !fontSizeRe.MatchString(o.FontSize) {
			return nil, fmt.Errorf("invalid font_size (e.g. \"14px\", \"11pt\")")
		}
		vars = append(vars, "body{font-size:"+o.FontSize+"}")
	}
	b.WriteString(strings.Join(vars, "\n"))
	b.WriteString("\n</style>\n")
	if o.CSS != "" {
		b.WriteString("<style>\n")
		b.WriteString(neutralizeStyleClose(o.CSS))
		b.WriteString("\n</style>\n")
	}
	dark := "0"
	if o.Theme == "github-dark" {
		dark = "1"
	}
	fmt.Fprintf(&b, "</head>\n<body data-dark=\"%s\">\n<article class=\"markdown-body\">\n", dark)
	b.WriteString(r.HTML)
	b.WriteString("\n</article>\n</body>\n</html>\n")

	// User HTML may contain anything; the CSP is what makes that safe. No
	// script runs unless it carries this request's nonce, nothing can be
	// framed or embedded, and forms/base/connections are disabled. Remote
	// images, fonts and stylesheets are allowed but every request is still
	// funnelled through the SSRF-checking fetcher by the renderer.
	csp := strings.Join([]string{
		"default-src 'none'",
		"script-src 'nonce-" + nonce + "'",
		"style-src 'self' 'unsafe-inline' https: http: data:",
		"img-src 'self' https: http: data: blob:",
		"font-src 'self' https: http: data:",
		"media-src 'none'", "object-src 'none'", "frame-src 'none'", "child-src 'none'", "worker-src 'none'",
		"connect-src 'none'", "form-action 'none'", "base-uri 'none'", "manifest-src 'none'",
	}, "; ")
	return &document{HTML: []byte(b.String()), CSP: csp, Title: title}, nil
}

var styleClose = regexp.MustCompile(`(?i)</(style)`)

// neutralizeStyleClose prevents user CSS from terminating its <style> element.
func neutralizeStyleClose(css string) string { return styleClose.ReplaceAllString(css, `<\/$1`) }

func mustAsset(name string) string {
	b, err := assetFS.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
