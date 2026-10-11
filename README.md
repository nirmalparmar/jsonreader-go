# common-service

A fast, single-binary Go API that powers the interactive tools on
[jsonreader.com](https://www.jsonreader.com):

| Feature | Transport | Endpoint |
|---|---|---|
| Online compilers (11 languages) | Server-sent events | `POST /v1/run/{lang}` |
| Markdown → PDF (any CSS) — **parked, off by default** | HTTP | `POST /v1/markdown/pdf` |
| Code Share (real-time collaborative editor) | WebSocket | `GET /v1/rooms/{id}/ws` |

```
cmd/server            entrypoint, wiring, graceful shutdown
internal/httpx        real-IP, CORS/origin guard, rate limiting, security headers, SSE writer
internal/sandbox      nsjail / docker / unsafe execution backends
internal/compiler     language registry, output streaming, SSE handler
internal/mdpdf        goldmark pipeline, themes, headless-Chrome renderer, SSRF-safe fetcher
internal/collab       OT engine, rooms, presence, chat, shared runs, persistence
deploy/               entrypoint (cgroup + procfs setup), nginx config
```

---

## Quick start

```bash
cp .env.example .env        # then edit ALLOWED_ORIGINS etc.
docker compose up -d --build
curl -s localhost:8080/healthz
```

Put `deploy/nginx.conf` in front for TLS (it disables buffering for SSE and
upgrades WebSockets), then point the frontend at it:

```bash
# jsonreader-react
NEXT_PUBLIC_API_BASE=https://api.jsonreader.com npm run build
```

### Local development

| Command | Sandbox | Use for |
|---|---|---|
| `make dev` | **none** (`SANDBOX=unsafe`) | Working on the API itself. Code runs as you. Never expose it. |
| `make dev-docker` | one container per run | Realistic isolation on macOS; slower (container start-up). |
| `make up` | nsjail in Docker | Production setup. |

`make test` runs `go vet` and the race-enabled test suite, including a
20 000-case OT convergence property test.

---

## API

All errors are JSON: `{"error": "code", "message": "human readable"}`.
Rate-limited responses are `429` with `Retry-After`.

### Compilers — `POST /v1/run/{lang}`

```json
{ "code": "print(input())", "stdin": "hello" }
```

Response is `text/event-stream`:

```
event: start   data: {"language":"python","version":"Python 3.11.2"}
event: stdout  data: {"text":"hello\n","phase":"run"}
event: stderr  data: {"stderr":"...","phase":"compile"|"run"}
event: exit    data: {"phase":"run","exit_code":0,"runtime_ms":42,"compile_ms":0,"timed_out":false,"truncated":false,"signal":"","note":""}
event: error   data: {"error":"internal","stderr":"..."}
```

Languages (enabled only if their toolchain passes a probe inside the sandbox at
start-up): `python`, `javascript` (`node`, `js`), `typescript`, `c`, `cpp`,
`go`, `java`, `rust`, `ruby`, `php`, `bash` (`sh`). `GET /v1/languages` lists
what is live, with versions and limits. The legacy paths
`/v1/python/stream`, `/v1/c/stream`, `/v1/node/stream` (and one per language)
are still served.

Measured in the production container (warm): bash 7 ms, PHP 31 ms, Python
~60 ms, Node ~70 ms, C ~90 ms, Rust ~200 ms, C++ ~220 ms, Java ~230 ms, Go
~470 ms (with a pre-warmed build cache), end to end.

### Markdown → PDF — `POST /v1/markdown/pdf`

> **Parked.** Disabled by default and Chromium is left out of the image. Re-enable with
> `docker compose build --build-arg WITH_PDF=true` (or `PDF_ENABLED=true` plus `CHROME_PATH` when running the
> binary directly). The matching frontend is kept in [parked/markdown-pdf-frontend](parked/markdown-pdf-frontend/README.md).

```json
{
  "markdown": "# Title\n...",
  "filename": "report",
  "options": {
    "theme": "github | github-dark | academic | modern | none",
    "code_theme": "github | dracula | monokai | nord | onedark | … (any chroma style)",
    "page_size": "A4 | Letter | Legal | A3 | A5 | B5 | Tabloid | Executive",
    "width": "180mm", "height": "250mm",
    "landscape": false,
    "margin": "18mm"  /* or "10mm 15mm" or {"top":"1in","right":"…","bottom":"…","left":"…"} */,
    "scale": 1,
    "print_background": true,
    "page_numbers": true,
    "header_html": "<div style='font-size:8px'><span class='title'></span></div>",
    "footer_html": "<div style='font-size:8px'><span class='pageNumber'></span>/<span class='totalPages'></span></div>",
    "page_ranges": "1-3",
    "toc": true,
    "line_numbers": false,
    "smart_quotes": false,
    "hard_breaks": false,
    "font_family": "Inter, sans-serif",
    "font_size": "14px",
    "css": "h1 { color: #4f46e5 } @page { size: A5; margin: 12mm }",
    "title": "PDF metadata title", "lang": "en"
  }
}
```

Returns `application/pdf`. Supported Markdown: GFM (tables, task lists,
strikethrough, autolinks), footnotes, definition lists, emoji shortcodes,
heading attributes (`# Title {#id .class style="…"}`), `$inline$` /
`$$display$$` / ` ```math ` LaTeX (KaTeX), ` ```mermaid ` diagrams,
GitHub alerts (`> [!NOTE]`, `TIP`, `IMPORTANT`, `WARNING`, `CAUTION`),
YAML front matter (`title:`), page breaks (`\pagebreak`, `\newpage`,
`<!-- pagebreak -->`), and raw HTML/`<style>`. The page is printed by headless
Chrome, so any CSS works: grid, flexbox, gradients, web fonts via `@import`,
`@page` size/margins, print media queries. `GET /v1/markdown/options` lists
themes, code themes and page sizes.

### Code Share

| | |
|---|---|
| `POST /v1/rooms` `{"language":"python","content":"…"}` | → `201 {"id","owner_token","language"}` |
| `GET /v1/rooms/{id}` | metadata (language, peers, locked, size, …) |
| `GET /v1/rooms/{id}/raw` | current document as `text/plain` |
| `GET /v1/rooms/{id}/ws` | collaboration channel |

WebSocket protocol (JSON text frames, `t` = type). The first client message must be
`join {sid, name, rev, owner_token}`; the server replies `init` with either the
full document or, on reconnect, the operations the client missed.

| Client → server | Server → client |
|---|---|
| `op {rev, seq, op, sel}` | `ack {rev}` · `op {rev, op, pid, sel}` |
| `sel {sel}` | `sel {pid, sel}` |
| `lang {lang}` · `name {name}` · `chat {text}` | `lang` · `peer` · `chat {msg}` · `join` · `leave` |
| `run {stdin}` · `stop` | `run_start` · `run_out {stream, data}` · `run_end {result}` |
| `lock {locked}` (owner only) | `lock {locked}` · `error {code, message}` |

Operations use the ot.js format (`n` retain, `-n` delete, `"s"` insert), with
offsets in UTF-16 code units so they map 1:1 onto Monaco/JavaScript. The server
is authoritative: it transforms each incoming op against everything committed
since the client's base revision, so every participant converges. The browser
client lives in `jsonreader-react/lib/collab/` and must keep the same tie-break
rule as `internal/collab/ot.go`.

Beyond codeshare.io, rooms add live named cursors and selections, chat, shared
code execution, an owner lock (view-only guests), reconnect without losing
edits, click-to-follow a collaborator, and persistence for 30 days of inactivity.

---

## Security model

**Untrusted code (nsjail, production)**: every execution gets
- fresh user, PID, mount, network, IPC and UTS namespaces; no network interfaces at all;
- a private, empty root with only read-only toolchain paths, a whitelisted handful of `/etc` files, a 64 MB `/tmp` and its own work dir;
- its own unprivileged host uid (61000+slot), so concurrent jails can't signal or read each other;
- cgroup v2 limits: memory (256 MB default, no swap), pids (fork bombs), CPU;
- rlimits: CPU time, file size, open files, no core dumps; plus wall-clock timeout, 1 MB output cap, and code/stdin size caps;
- a seccomp deny-list (ptrace, mount, unshare/setns, nested user namespaces via `clone`, bpf, perf, keyrings, kexec, modules, …), with no capabilities and `no_new_privs`.

Verified against: outbound TCP/DNS, reading host files, writing outside the jail, fork
bombs, 2 GB allocations, disk filling, `unshare`/`ptrace`/`mount`, `kill -9 -1`,
CPU spins, and cross-run snooping. Timeouts SIGTERM nsjail so it removes its own
cgroup; a sweeper removes any stragglers.

The container needs `CAP_SYS_ADMIN` and unconfined seccomp/AppArmor so nsjail can
build namespaces (see `docker-compose.yml`; `privileged` is **not** required).
`deploy/entrypoint.sh` delegates cgroup v2 controllers and mounts a clean
read-only procfs in a root-only directory: Docker's masked `/proc` would otherwise
stop jails from mounting their own `/proc`.

**Markdown → PDF**: raw HTML is allowed (that's how "any styling" works), so the
print page is locked down instead:
- a per-request CSP nonce; user `<script>`, inline handlers and `javascript:` URLs never run;
- no frames, objects, forms, `<base>` or network connections from scripts;
- every request Chrome makes is intercepted. The document and the vendored assets (KaTeX, Mermaid) are served from memory. Remote images, fonts and CSS go through a Go fetcher that only connects to public IPs (checked at dial time, so DNS rebinding and redirects are covered), only on ports 80/443, with per-file and per-document size/count budgets;
- as a backstop Chrome's resolver maps every host to NOTFOUND and its proxy is a dead port;
- Chrome runs as an unprivileged user with its own sandbox enabled.

**HTTP**: an exact-origin CORS allow-list that *rejects* foreign origins (not just
omits headers); WebSocket origin verification; per-IP token-bucket limits per feature;
`X-Real-IP` honoured only from `TRUSTED_PROXIES`; body size limits; strict
security headers; panic recovery; request ids.

---

## Image size

The server itself is 17 MB; the image is almost entirely language toolchains, already trimmed
(minimal jlink'd Java 21 runtime, Go without tests/sources/profilers, no sanitizer runtimes, no docs).
Pick the languages you need with the `LANGS` build arg in `docker-compose.yml`:

| `LANGS` | Image |
|---|---|
| all 11 (default) | 1.65 GB |
| everything except `rust` | 1.14 GB |
| `python,javascript,c` | 0.6 GB |

Languages left out are simply not offered by `/v1/languages`. TypeScript comes free with
JavaScript (same Node binary) and Bash with the base OS.

## Configuration

See [.env.example](.env.example) for every variable and its default. The ones you
most likely need:

- `ALLOWED_ORIGINS` — your site origins.
- `TRUSTED_PROXIES` — must include the Docker bridge range (`172.16.0.0/12`) when nginx reaches the container through a published port, otherwise all users share one rate-limit bucket.
- `COMPILER_MAX_CONCURRENT`, `COMPILER_RUN_TIMEOUT`, `COMPILER_MEMORY_MB`.
- `PDF_ALLOW_REMOTE` — set `false` to forbid documents from loading any remote resource.
