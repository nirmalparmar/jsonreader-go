package mdpdf

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	emoji "github.com/yuin/goldmark-emoji"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	gmhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"go.abhg.dev/goldmark/toc"
)

type mdOptions struct {
	CodeTheme   string
	LineNumbers bool
	TOC         bool
	SmartQuotes bool
	HardBreaks  bool
}

type rendered struct {
	HTML       string
	CodeCSS    string
	HasMath    bool
	HasMermaid bool
	Title      string // from front matter or first H1
}

// renderMarkdown converts Markdown to an HTML fragment. Raw HTML is passed
// through on purpose (custom styling, <style> blocks, inline styles); the
// print page isolates it with a strict CSP instead of stripping it.
func renderMarkdown(src string, o mdOptions) (*rendered, error) {
	front, body := splitFrontMatter(src)
	body = expandPageBreaks(body)

	style := styles.Get(o.CodeTheme)
	if style == nil || style == styles.Fallback {
		style = styles.Get("github")
	}
	code := &codeRenderer{style: style, lineNumbers: o.LineNumbers}

	exts := []goldmark.Extender{
		extension.GFM, // tables, task lists, strikethrough, autolinks
		extension.Footnote,
		extension.DefinitionList,
		emoji.Emoji,
		mathExtension{},
	}
	if o.SmartQuotes {
		exts = append(exts, extension.Typographer)
	}
	if o.TOC {
		exts = append(exts, &toc.Extender{Title: "Contents", MinDepth: 1, MaxDepth: 3, ListID: "toc", TitleID: "toc-title"})
	}
	rendererOpts := []renderer.Option{gmhtml.WithUnsafe()}
	if o.HardBreaks {
		rendererOpts = append(rendererOpts, gmhtml.WithHardWraps())
	}
	md := goldmark.New(
		goldmark.WithExtensions(exts...),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithAttribute(), // # Heading {#id .class style="..."}
		),
		goldmark.WithRendererOptions(append(rendererOpts,
			renderer.WithNodeRenderers(util.Prioritized(code, 100)))...),
	)

	var buf bytes.Buffer
	if err := md.Convert([]byte(body), &buf); err != nil {
		return nil, err
	}

	var css bytes.Buffer
	_ = chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(true)).WriteCSS(&css, style)

	out := &rendered{
		HTML:       buf.String(),
		CodeCSS:    css.String(),
		HasMath:    code.math || strings.Contains(buf.String(), `class="math `),
		HasMermaid: code.mermaid,
		Title:      front["title"],
	}
	if out.Title == "" {
		// First real H1, skipping the generated table-of-contents heading.
		for _, m := range firstH1.FindAllStringSubmatch(out.HTML, 8) {
			if !strings.Contains(m[1], `id="toc-title"`) {
				out.Title = strings.TrimSpace(html.UnescapeString(stripTags.ReplaceAllString(m[2], "")))
				break
			}
		}
	}
	return out, nil
}

var (
	firstH1   = regexp.MustCompile(`(?s)<h1([^>]*)>(.*?)</h1>`)
	stripTags = regexp.MustCompile(`<[^>]*>`)
)

// splitFrontMatter removes a leading YAML front-matter block and returns its
// simple "key: value" pairs.
func splitFrontMatter(src string) (map[string]string, string) {
	meta := map[string]string{}
	s := strings.TrimPrefix(src, "\uFEFF")
	if !strings.HasPrefix(s, "---\n") && !strings.HasPrefix(s, "---\r\n") {
		return meta, src
	}
	lines := strings.SplitAfter(s, "\n")
	for i := 1; i < len(lines) && i < 200; i++ {
		l := strings.TrimRight(lines[i], "\r\n")
		if l == "---" || l == "..." {
			return meta, strings.Join(lines[i+1:], "")
		}
		if k, v, ok := strings.Cut(l, ":"); ok && !strings.HasPrefix(k, " ") {
			meta[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return map[string]string{}, src
}

var pageBreakLine = regexp.MustCompile(`^\s*(\\pagebreak|\\newpage|<!--\s*(pagebreak|page-break|newpage)\s*-->)\s*$`)

// expandPageBreaks turns \pagebreak, \newpage and <!-- pagebreak --> lines
// (outside fenced code) into explicit page-break elements.
func expandPageBreaks(src string) string {
	lines := strings.Split(src, "\n")
	fence := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if fence == "" && (strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")) {
			fence = t[:3]
			continue
		}
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			continue
		}
		if pageBreakLine.MatchString(l) {
			lines[i] = "\n<div class=\"page-break\"></div>\n"
		}
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Code blocks: chroma highlighting, plus ```mermaid and ```math fences.

type codeRenderer struct {
	style       *chroma.Style
	lineNumbers bool
	mermaid     bool
	math        bool
}

func (r *codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.render)
	reg.Register(ast.KindCodeBlock, r.render)
}

func (r *codeRenderer) render(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var lang, info string
	if f, ok := n.(*ast.FencedCodeBlock); ok {
		lang = strings.ToLower(string(f.Language(src)))
		if f.Info != nil {
			info = string(f.Info.Segment.Value(src))
		}
	}
	var code strings.Builder
	lines := n.Lines()
	for i := range lines.Len() {
		seg := lines.At(i)
		code.Write(seg.Value(src))
	}

	switch lang {
	case "mermaid":
		r.mermaid = true
		fmt.Fprintf(w, "<pre class=\"mermaid\">%s</pre>\n", html.EscapeString(code.String()))
		return ast.WalkSkipChildren, nil
	case "math", "latex", "katex", "tex":
		r.math = true
		fmt.Fprintf(w, "<div class=\"math math-display\">%s</div>\n", html.EscapeString(code.String()))
		return ast.WalkSkipChildren, nil
	}

	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	ln := r.lineNumbers || strings.Contains(info, "linenos") || strings.Contains(info, "showLineNumbers")
	f := chromahtml.New(chromahtml.WithClasses(true), chromahtml.WithLineNumbers(ln), chromahtml.TabWidth(4))
	it, err := lexer.Tokenise(nil, code.String())
	if err != nil {
		fmt.Fprintf(w, "<pre><code>%s</code></pre>\n", html.EscapeString(code.String()))
		return ast.WalkSkipChildren, nil
	}
	label := ""
	if lang != "" {
		label = fmt.Sprintf(" data-lang=\"%s\"", html.EscapeString(lang))
	}
	fmt.Fprintf(w, "<div class=\"code-block\"%s>", label)
	if err := f.Format(w, r.style, it); err != nil {
		return ast.WalkStop, err
	}
	_, _ = w.WriteString("</div>\n")
	return ast.WalkSkipChildren, nil
}

// ---------------------------------------------------------------------------
// Math: $inline$, $$display$$ (inline or as a block spanning lines). Content
// is kept verbatim (no emphasis/escape processing) and typeset by KaTeX in the
// print page.

var kindMath = ast.NewNodeKind("Math")

type mathNode struct {
	ast.BaseInline
	display bool
	content []byte
}

func (n *mathNode) Kind() ast.NodeKind         { return kindMath }
func (n *mathNode) Dump(src []byte, level int) { ast.DumpHelper(n, src, level, nil, nil) }
func (n *mathNode) IsRaw() bool                { return true }

var kindMathBlock = ast.NewNodeKind("MathBlock")

type mathBlock struct{ ast.BaseBlock }

func (n *mathBlock) Kind() ast.NodeKind         { return kindMathBlock }
func (n *mathBlock) Dump(src []byte, level int) { ast.DumpHelper(n, src, level, nil, nil) }
func (n *mathBlock) IsRaw() bool                { return true }

type mathExtension struct{}

func (mathExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithInlineParsers(util.Prioritized(mathInlineParser{}, 150)),
		parser.WithBlockParsers(util.Prioritized(mathBlockParser{}, 90)),
	)
	m.Renderer().AddOptions(renderer.WithNodeRenderers(util.Prioritized(mathRenderer{}, 150)))
}

type mathInlineParser struct{}

func (mathInlineParser) Trigger() []byte { return []byte{'$'} }

func (mathInlineParser) Parse(_ ast.Node, block text.Reader, _ parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) < 2 {
		return nil
	}
	if line[1] == '$' { // $$...$$ on one line
		end := bytes.Index(line[2:], []byte("$$"))
		if end <= 0 {
			return nil
		}
		block.Advance(end + 4)
		return &mathNode{display: true, content: bytes.TrimSpace(line[2 : 2+end])}
	}
	// $...$: no space after the opener, none before the closer, and the
	// closer must not be followed by a digit (so "$5 and $10" stays text).
	if isSpace(line[1]) {
		return nil
	}
	for i := 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '$':
			if isSpace(line[i-1]) || (i+1 < len(line) && line[i+1] >= '0' && line[i+1] <= '9') {
				continue
			}
			block.Advance(i + 1)
			return &mathNode{content: line[1:i]}
		}
	}
	return nil
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

type mathBlockParser struct{}

func (mathBlockParser) Trigger() []byte { return []byte{'$'} }

func (mathBlockParser) Open(_ ast.Node, reader text.Reader, _ parser.Context) (ast.Node, parser.State) {
	line, seg := reader.PeekLine()
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("$$")) {
		return nil, parser.NoChildren
	}
	rest := bytes.TrimSpace(t[2:])
	// "$$ x $$" alone on a line is handled by the inline parser.
	if len(rest) > 0 && bytes.HasSuffix(rest, []byte("$$")) {
		return nil, parser.NoChildren
	}
	node := &mathBlock{}
	if len(rest) > 0 {
		start := seg.Start + bytes.Index(line, []byte("$$")) + 2
		node.Lines().Append(text.NewSegment(start, seg.Stop))
	}
	reader.Advance(seg.Len() - 1)
	return node, parser.NoChildren
}

func (mathBlockParser) Continue(node ast.Node, reader text.Reader, _ parser.Context) parser.State {
	line, seg := reader.PeekLine()
	t := bytes.TrimSpace(line)
	if bytes.HasSuffix(t, []byte("$$")) {
		if pre := bytes.TrimSuffix(t, []byte("$$")); len(bytes.TrimSpace(pre)) > 0 {
			idx := bytes.LastIndex(line, []byte("$$"))
			node.Lines().Append(text.NewSegment(seg.Start, seg.Start+idx))
		}
		reader.Advance(seg.Len() - 1)
		return parser.Close
	}
	node.Lines().Append(seg)
	reader.Advance(seg.Len() - 1)
	return parser.Continue | parser.NoChildren
}

func (mathBlockParser) Close(ast.Node, text.Reader, parser.Context) {}
func (mathBlockParser) CanInterruptParagraph() bool                 { return true }
func (mathBlockParser) CanAcceptIndentedLine() bool                 { return false }

type mathRenderer struct{}

func (mathRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(kindMath, func(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			m := n.(*mathNode)
			cls := "math math-inline"
			if m.display {
				cls = "math math-display"
			}
			fmt.Fprintf(w, `<span class="%s">%s</span>`, cls, html.EscapeString(string(m.content)))
		}
		return ast.WalkSkipChildren, nil
	})
	reg.Register(kindMathBlock, func(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			var b strings.Builder
			lines := n.Lines()
			for i := range lines.Len() {
				seg := lines.At(i)
				b.Write(seg.Value(src))
			}
			fmt.Fprintf(w, "<div class=\"math math-display\">%s</div>\n", html.EscapeString(strings.TrimSpace(b.String())))
		}
		return ast.WalkSkipChildren, nil
	})
}
