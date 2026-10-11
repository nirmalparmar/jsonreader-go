"use client";

import { useEffect, useState } from "react";
import { toast } from "sonner";
import { FileDown, Loader2 } from "lucide-react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Label } from "@/components/ui/label";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { API_BASE, apiError } from "@/lib/api";

const THEMES = [
  ["github", "GitHub"],
  ["github-dark", "GitHub Dark"],
  ["academic", "Academic (LaTeX style)"],
  ["modern", "Modern"],
  ["none", "Unstyled (bring your own CSS)"],
];
const CODE_THEMES = ["github", "github-dark", "dracula", "monokai", "nord", "onedark", "solarized-light", "vs", "xcode", "catppuccin-mocha"];
const PAGE_SIZES = ["A4", "Letter", "Legal", "A3", "A5", "B5", "Tabloid", "Executive"];
const MARGINS = [
  ["0mm", "None"],
  ["10mm", "Narrow (10 mm)"],
  ["18mm", "Normal (18 mm)"],
  ["25mm", "Wide (25 mm)"],
];

const DEFAULTS = {
  theme: "github",
  code_theme: "github",
  page_size: "A4",
  landscape: false,
  margin: "18mm",
  page_numbers: true,
  toc: false,
  line_numbers: false,
  print_background: true,
  font_size: "",
  css: "",
};

const STORAGE_KEY = "jr:md-pdf-options";

function Field({ label, children }) {
  return (
    <div className="space-y-1.5">
      <Label className="text-xs text-muted-foreground">{label}</Label>
      {children}
    </div>
  );
}

function Toggle({ id, label, checked, onChange }) {
  return (
    <label htmlFor={id} className="flex cursor-pointer items-center justify-between gap-3 rounded-md border border-border px-3 py-2">
      <span className="text-sm">{label}</span>
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
    </label>
  );
}

/** Renders Markdown to PDF on the server (headless Chrome) and downloads it. */
export default function PdfExportDialog({ open, onOpenChange, markdown, defaultFilename = "document" }) {
  const [opts, setOpts] = useState(DEFAULTS);
  const [filename, setFilename] = useState(defaultFilename);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    try {
      const saved = JSON.parse(localStorage.getItem(STORAGE_KEY) || "null");
      if (saved) setOpts({ ...DEFAULTS, ...saved });
    } catch {}
  }, []);

  const set = (k) => (v) => {
    setOpts((o) => {
      const next = { ...o, [k]: v };
      // Keep code colours readable when switching between light and dark.
      if (k === "theme" && v === "github-dark" && o.code_theme === "github") next.code_theme = "github-dark";
      if (k === "theme" && v !== "github-dark" && o.code_theme === "github-dark") next.code_theme = "github";
      try { localStorage.setItem(STORAGE_KEY, JSON.stringify(next)); } catch {}
      return next;
    });
  };

  const exportPdf = async () => {
    if (!markdown?.trim()) {
      toast.error("Nothing to export — the document is empty.");
      return;
    }
    setBusy(true);
    try {
      const { font_size, ...rest } = opts;
      const res = await fetch(`${API_BASE}/v1/markdown/pdf`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          markdown,
          filename,
          options: { ...rest, ...(font_size ? { font_size } : {}) },
        }),
      });
      if (!res.ok) throw await apiError(res);
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `${(filename || "document").replace(/\.pdf$/i, "")}.pdf`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 10_000);
      toast.success("PDF downloaded");
      onOpenChange(false);
    } catch (err) {
      toast.error(err.message || "Could not create the PDF");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Export Markdown to PDF</DialogTitle>
          <DialogDescription>
            Rendered with a real browser engine: GitHub Flavored Markdown, math, Mermaid diagrams, syntax highlighting,
            raw HTML and any CSS you add.
          </DialogDescription>
        </DialogHeader>

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <Field label="File name">
            <Input value={filename} onChange={(e) => setFilename(e.target.value)} placeholder="document" />
          </Field>
          <Field label="Theme">
            <Select value={opts.theme} onValueChange={set("theme")}>
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                {THEMES.map(([v, l]) => <SelectItem key={v} value={v}>{l}</SelectItem>)}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Code highlighting">
            <Select value={opts.code_theme} onValueChange={set("code_theme")}>
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                {CODE_THEMES.map((v) => <SelectItem key={v} value={v}>{v}</SelectItem>)}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Page size">
            <Select value={opts.page_size} onValueChange={set("page_size")}>
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                {PAGE_SIZES.map((v) => <SelectItem key={v} value={v}>{v}</SelectItem>)}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Orientation">
            <Select value={opts.landscape ? "landscape" : "portrait"} onValueChange={(v) => set("landscape")(v === "landscape")}>
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="portrait">Portrait</SelectItem>
                <SelectItem value="landscape">Landscape</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          <Field label="Margins">
            <Select value={opts.margin} onValueChange={set("margin")}>
              <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
              <SelectContent>
                {MARGINS.map(([v, l]) => <SelectItem key={v} value={v}>{l}</SelectItem>)}
              </SelectContent>
            </Select>
          </Field>
          <Field label="Base font size (optional)">
            <Input value={opts.font_size} onChange={(e) => set("font_size")(e.target.value.trim())} placeholder="e.g. 14px or 11pt" />
          </Field>
        </div>

        <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          <Toggle id="pdf-pn" label="Page numbers" checked={opts.page_numbers} onChange={set("page_numbers")} />
          <Toggle id="pdf-toc" label="Table of contents" checked={opts.toc} onChange={set("toc")} />
          <Toggle id="pdf-ln" label="Line numbers in code" checked={opts.line_numbers} onChange={set("line_numbers")} />
          <Toggle id="pdf-bg" label="Print backgrounds" checked={opts.print_background} onChange={set("print_background")} />
        </div>

        <Field label="Custom CSS (applied last — overrides the theme)">
          <Textarea
            value={opts.css}
            onChange={(e) => set("css")(e.target.value)}
            rows={4}
            spellCheck={false}
            className="font-mono text-xs"
            placeholder={"@import url('https://fonts.googleapis.com/css2?family=Inter');\nbody { font-family: Inter, sans-serif; }\nh1 { color: #4f46e5; }\n@page { size: A4; margin: 20mm; }"}
          />
        </Field>
        <p className="text-xs text-muted-foreground">
          Tip: use <code>\pagebreak</code> or <code>&lt;!-- pagebreak --&gt;</code> on its own line to start a new page, and
          <code> &gt; [!NOTE]</code> for GitHub-style callouts.
        </p>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={busy}>Cancel</Button>
          <Button onClick={exportPdf} disabled={busy}>
            {busy ? <Loader2 className="mr-2 h-4 w-4 animate-spin" /> : <FileDown className="mr-2 h-4 w-4" />}
            {busy ? "Rendering…" : "Download PDF"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
