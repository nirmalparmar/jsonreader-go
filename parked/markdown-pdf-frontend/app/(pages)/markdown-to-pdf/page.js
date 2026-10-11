import ToolPage from "@/app/components/tool/ToolPage";
import MarkdownEditor from "@/app/components/markdown/MarkdownEditor";

const SAMPLE = `---
title: Quarterly Report
---
# Quarterly Report

Click **Export PDF** in the toolbar to download this document as a styled PDF.

> [!TIP]
> GitHub-style callouts, tables, task lists and footnotes all carry over to the PDF.[^1]

## Results

| Metric      | Q1    | Q2    | Change |
|:------------|------:|------:|-------:|
| Revenue     | $1.2M | $1.5M |  +25 % |
| Active users| 8,400 | 11,900|  +42 % |

Growth follows $r = \\frac{v_2 - v_1}{v_1}$, and over time:

$$
V(t) = V_0 \\, e^{kt}
$$

## Architecture

\`\`\`mermaid
flowchart LR
  A[Markdown] --> B(Renderer) --> C[[PDF]]
\`\`\`

\`\`\`python
def growth(v1, v2):
    return (v2 - v1) / v1
\`\`\`

- [x] Ship the converter
- [ ] Celebrate :tada:

<div style="padding:12px 16px;border-radius:10px;background:linear-gradient(90deg,#eef2ff,#fdf2f8)">
  Raw HTML and inline styles are supported too.
</div>

\\pagebreak

## Appendix

Everything after \`\\pagebreak\` starts on a new page.

[^1]: Footnotes are collected at the end of the document.
`;

const FEATURES = [
  ["Real browser rendering", "PDFs are produced by a headless browser engine, so any CSS works — grid, flexbox, web fonts, @page rules."],
  ["Themes", "GitHub, GitHub Dark, Academic (LaTeX look), Modern, or unstyled with your own CSS."],
  ["Math & diagrams", "LaTeX math via KaTeX and Mermaid flowcharts, sequence diagrams and more."],
  ["Syntax highlighting", "Dozens of code colour schemes, with optional line numbers."],
  ["Print controls", "Page size, orientation, margins, page numbers, page breaks and a table of contents."],
  ["Callouts & HTML", "GitHub alerts, footnotes, task lists, definition lists, emoji and raw HTML."],
];

const FAQ = [
  { question: "Is the Markdown to PDF converter free?", answer: "Yes. It is free with no signup and no watermark." },
  { question: "Can I use my own CSS?", answer: "Yes. Open Export PDF and paste CSS into the Custom CSS box — it is applied after the theme, so it overrides anything. @import of web fonts and @page rules are supported." },
  { question: "How do I start a new page?", answer: "Put \\pagebreak, \\newpage or <!-- pagebreak --> on its own line." },
  { question: "Are math and Mermaid diagrams supported?", answer: "Yes. Write $inline$ or $$block$$ LaTeX math and ```mermaid code blocks; both are rendered into the PDF." },
  { question: "Is my document stored?", answer: "No. The Markdown is sent over HTTPS, converted, and discarded immediately — nothing is saved." },
];

const HOWTO = [
  "Write or paste your Markdown into the editor.",
  "Check the live preview on the right.",
  "Click Export PDF and pick a theme, page size and options.",
  "Download your styled PDF.",
];

export default function MarkdownToPdfPage() {
  return (
    <ToolPage
      slug="/markdown-to-pdf"
      name="Markdown to PDF"
      h1="Markdown to PDF Converter — Beautiful, Styled PDFs in One Click"
      description="Convert Markdown to PDF with themes, math, Mermaid diagrams, syntax highlighting and custom CSS."
      lead="Turn Markdown into polished PDFs. Choose a theme, add your own CSS, and export documents with math, diagrams, code highlighting, page numbers and a table of contents."
      whatIs="Markdown to PDF conversion renders your Markdown — headings, tables, code, math and diagrams — into a print-ready PDF document. This converter uses a real browser engine, so the PDF looks exactly like a styled web page and honours any CSS you provide."
      howTo={HOWTO}
      features={FEATURES}
      faq={FAQ}
      related={[
        { href: "/markdown-editor", label: "Markdown Editor" },
        { href: "/code-share", label: "Code Share" },
        { href: "/json-formatter", label: "JSON Formatter" },
        { href: "/word-counter", label: "Word Counter" },
      ]}
      tool={<MarkdownEditor initialValue={SAMPLE} />}
    />
  );
}
