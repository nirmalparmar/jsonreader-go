# Parked: Markdown → PDF frontend

Removed from jsonreader-react on 2026-10-11 while the feature is on hold. To restore:

1. Copy `app/(pages)/markdown-to-pdf/` and `app/components/markdown/PdfExportDialog.js` back into jsonreader-react.
2. In `app/components/markdown/MarkdownEditor.js`: accept an `initialValue` prop, add a `pdfOpen` state,
   an `{ id: "pdf", label: "Export PDF", icon: FileDown, onClick: () => setPdfOpen(true) }` toolbar action,
   and render `<PdfExportDialog open={pdfOpen} onOpenChange={setPdfOpen} markdown={md} />`.
3. Re-add the "Markdown to PDF" links in the Navbar (utilities) and Footer (utilities).
4. Build the API image with `--build-arg WITH_PDF=true` (installs Chromium and enables `/v1/markdown/pdf`).

Open idea from the discussion: add an in-browser "Print / Save as PDF" path and make the
preview renderer match the server (Mermaid, GitHub alerts, page breaks).
