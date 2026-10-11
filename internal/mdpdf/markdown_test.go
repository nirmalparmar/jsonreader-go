package mdpdf

import (
	"strings"
	"testing"
)

func render(t *testing.T, src string, o mdOptions) *rendered {
	t.Helper()
	if o.CodeTheme == "" {
		o.CodeTheme = "github"
	}
	r, err := renderMarkdown(src, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMath(t *testing.T) {
	r := render(t, "Euler $e^{i\\pi}+1=0$ costs $5 and $10.\n\n$$\na_b_c\n$$\n", mdOptions{})
	for _, want := range []string{
		`<span class="math math-inline">e^{i\pi}+1=0</span>`,
		"costs $5 and $10.",
		`<div class="math math-display">a_b_c</div>`,
	} {
		if !strings.Contains(r.HTML, want) {
			t.Errorf("missing %q in\n%s", want, r.HTML)
		}
	}
	if !r.HasMath {
		t.Error("HasMath = false")
	}
}

func TestMermaidAndCode(t *testing.T) {
	r := render(t, "```mermaid\ngraph LR; A-->B\n```\n\n```go\nfunc main() {}\n```\n", mdOptions{})
	if !r.HasMermaid || !strings.Contains(r.HTML, `<pre class="mermaid">graph LR; A--&gt;B`) {
		t.Errorf("mermaid not rendered: %s", r.HTML)
	}
	if !strings.Contains(r.HTML, `class="chroma"`) || !strings.Contains(r.HTML, `data-lang="go"`) {
		t.Errorf("code not highlighted: %s", r.HTML)
	}
}

func TestTitleSkipsTOC(t *testing.T) {
	r := render(t, "# Real Title\n\n## Section\n", mdOptions{TOC: true})
	if r.Title != "Real Title" {
		t.Errorf("title = %q", r.Title)
	}
	r = render(t, "---\ntitle: From Front Matter\n---\n# Heading\n", mdOptions{})
	if r.Title != "From Front Matter" || strings.Contains(r.HTML, "title:") {
		t.Errorf("front matter: title=%q html=%s", r.Title, r.HTML)
	}
}

func TestPageBreaksOutsideFences(t *testing.T) {
	r := render(t, "a\n\n\\pagebreak\n\n```\n\\pagebreak\n```\n", mdOptions{})
	if strings.Count(r.HTML, `class="page-break"`) != 1 {
		t.Errorf("expected exactly one page break: %s", r.HTML)
	}
}

func TestUserCSSCannotCloseStyle(t *testing.T) {
	doc, err := buildDocument("# x", &Options{Theme: "github", CodeTheme: "github", CSS: "body{}</style><script>alert(1)</script>"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(doc.HTML), "</style><script>") {
		t.Error("user CSS escaped its <style> element")
	}
	if !strings.Contains(doc.CSP, "script-src 'nonce-") {
		t.Error("CSP missing nonce")
	}
}
