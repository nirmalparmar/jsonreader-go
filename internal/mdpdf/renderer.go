// Package mdpdf renders Markdown to PDF with headless Chrome, so any CSS the
// browser understands (grid, flexbox, web fonts, @page rules, print media
// queries) is honoured in the output.
package mdpdf

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/nirmalparmar/common-service/internal/config"
)

// The print page lives on a reserved, never-resolvable host. Chrome never
// performs a lookup for it: every request is intercepted and answered here.
const docOrigin = "https://document.invalid"

var ErrBusy = errors.New("renderer busy, try again shortly")

type Renderer struct {
	cfg     config.PDFConfig
	log     *slog.Logger
	sem     chan struct{}
	fetcher *safeFetcher

	mu      sync.Mutex
	browser context.Context
	stop    context.CancelFunc
}

func NewRenderer(cfg config.PDFConfig, log *slog.Logger) *Renderer {
	return &Renderer{
		cfg:     cfg,
		log:     log,
		sem:     make(chan struct{}, max(cfg.MaxConcurrent, 1)),
		fetcher: newSafeFetcher(10 << 20),
	}
}

func (r *Renderer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		r.stop()
		r.stop, r.browser = nil, nil
	}
}

// browserCtx returns the shared browser, (re)starting Chrome if needed.
func (r *Renderer) browserCtx() (context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.browser != nil && r.browser.Err() == nil {
		return r.browser, nil
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.Flag("hide-scrollbars", true),
		chromedp.Flag("mute-audio", true),
		chromedp.Flag("font-render-hinting", "none"),
		chromedp.Flag("block-new-web-contents", true),
		chromedp.Flag("disable-notifications", true),
		chromedp.Flag("disable-domain-reliability", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("no-pings", true),
		// Defence in depth: should any request ever slip past interception,
		// it can neither resolve a name nor reach a host.
		chromedp.Flag("host-resolver-rules", "MAP * ~NOTFOUND"),
		chromedp.ProxyServer("http://127.0.0.1:9"),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		chromedp.Flag("js-flags", "--max-old-space-size=512"),
	)
	if r.cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(r.cfg.ChromePath))
	}
	if r.cfg.ChromeNoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	if uid := r.cfg.ChromeUID; uid > 0 && os.Geteuid() == 0 {
		// The server runs as root (nsjail needs it); Chrome does not.
		dir, err := os.MkdirTemp("", "chrome-profile-")
		if err != nil {
			return nil, err
		}
		if err := os.Chown(dir, uid, uid); err != nil {
			return nil, err
		}
		opts = append(opts,
			chromedp.UserDataDir(dir),
			chromedp.Env("HOME="+dir),
			chromedp.ModifyCmdFunc(func(cmd *exec.Cmd) {
				if cmd.SysProcAttr == nil {
					cmd.SysProcAttr = &syscall.SysProcAttr{}
				}
				cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}
			}),
		)
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	bctx, bcancel := chromedp.NewContext(allocCtx)
	// The browser is owned by the context of the first Run, so it must be
	// bctx itself (a derived timeout context would kill Chrome on return).
	timer := time.AfterFunc(30*time.Second, bcancel)
	err := chromedp.Run(bctx)
	timer.Stop()
	if err != nil {
		bcancel()
		allocCancel()
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	r.browser = bctx
	r.stop = func() { bcancel(); allocCancel() }
	r.log.Info("chrome started")
	return bctx, nil
}

// Warm starts Chrome ahead of the first request.
func (r *Renderer) Warm() error { _, err := r.browserCtx(); return err }

type result struct {
	PDF   []byte
	Title string
	Warns []string
}

func (r *Renderer) Render(ctx context.Context, markdown string, opts Options) (*result, error) {
	ps, err := opts.normalize()
	if err != nil {
		return nil, &inputError{err}
	}
	doc, err := buildDocument(markdown, &opts)
	if err != nil {
		return nil, &inputError{err}
	}

	wait := time.NewTimer(10 * time.Second)
	defer wait.Stop()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-wait.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	bctx, err := r.browserCtx()
	if err != nil {
		return nil, err
	}
	out, err := r.print(ctx, bctx, doc, ps)
	if err != nil && ctx.Err() == nil && bctx.Err() != nil {
		// Browser died mid-render; retry once on a fresh instance.
		r.log.Warn("chrome crashed, restarting", "err", err)
		if bctx, err = r.browserCtx(); err == nil {
			out, err = r.print(ctx, bctx, doc, ps)
		}
	}
	return out, err
}

func (r *Renderer) print(ctx, bctx context.Context, doc *document, ps printSettings) (*result, error) {
	tab, closeTab := chromedp.NewContext(bctx)
	defer closeTab()
	tab, cancel := context.WithTimeout(tab, r.cfg.RenderTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel) // client went away
	defer stop()

	budget := &fetchBudget{maxReq: 100, maxBytes: int64(r.cfg.MaxRemoteBytes)}
	chromedp.ListenTarget(tab, func(ev any) {
		if e, ok := ev.(*fetch.EventRequestPaused); ok {
			go r.intercept(tab, e, doc, budget)
		}
	})

	var pdf []byte
	var warns []string
	err := chromedp.Run(tab,
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}),
		chromedp.Navigate(docOrigin+"/"),
		chromedp.Poll(`window.__renderDone === true`, nil, chromedp.WithPollingInterval(25*time.Millisecond)),
		chromedp.Evaluate(`window.__renderErrors || []`, &warns),
		chromedp.ActionFunc(func(ctx context.Context) error {
			p := page.PrintToPDF().
				WithPaperWidth(ps.width).WithPaperHeight(ps.height).
				WithLandscape(ps.landscape).
				WithMarginTop(ps.marginT).WithMarginRight(ps.marginR).
				WithMarginBottom(ps.marginB).WithMarginLeft(ps.marginL).
				WithScale(ps.scale).
				WithPrintBackground(ps.background).
				WithPreferCSSPageSize(true). // honour @page { size: ... } in user CSS
				WithGenerateDocumentOutline(true).
				WithGenerateTaggedPDF(true).
				WithTransferMode(page.PrintToPDFTransferModeReturnAsBase64)
			if ps.headerFooter {
				p = p.WithDisplayHeaderFooter(true).WithHeaderTemplate(ps.header).WithFooterTemplate(ps.footer)
			}
			if ps.pageRanges != "" {
				p = p.WithPageRanges(ps.pageRanges)
			}
			var err error
			pdf, _, err = p.Do(ctx)
			return err
		}),
	)
	if err != nil {
		if errors.Is(tab.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("render timed out after %s", r.cfg.RenderTimeout)
		}
		return nil, err
	}
	return &result{PDF: pdf, Title: doc.Title, Warns: warns}, nil
}

// intercept answers every request the print page makes: the document and our
// embedded assets are served from memory; remote http(s) subresources go
// through the SSRF-safe fetcher; everything else is refused.
func (r *Renderer) intercept(tab context.Context, e *fetch.EventRequestPaused, doc *document, budget *fetchBudget) {
	c := chromedp.FromContext(tab)
	if c == nil || c.Target == nil {
		return
	}
	exec := cdp.WithExecutor(tab, c.Target)
	fail := func() { _ = fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(exec) }
	fulfill := func(status int64, ctype string, body []byte, extra ...*fetch.HeaderEntry) {
		h := append([]*fetch.HeaderEntry{
			{Name: "Content-Type", Value: ctype},
			{Name: "Cache-Control", Value: "no-store"},
		}, extra...)
		_ = fetch.FulfillRequest(e.RequestID, status).
			WithResponseHeaders(h).
			WithBody(base64.StdEncoding.EncodeToString(body)).
			Do(exec)
	}

	u, err := url.Parse(e.Request.URL)
	if err != nil {
		fail()
		return
	}
	if u.Scheme+"://"+u.Host == docOrigin {
		switch {
		case u.Path == "/" && e.ResourceType == network.ResourceTypeDocument:
			fulfill(200, "text/html; charset=utf-8", doc.HTML,
				&fetch.HeaderEntry{Name: "Content-Security-Policy", Value: doc.CSP},
				&fetch.HeaderEntry{Name: "X-Content-Type-Options", Value: "nosniff"})
		case strings.HasPrefix(u.Path, "/assets/") && !strings.HasPrefix(u.Path, "/assets/themes/"):
			name := path.Clean(strings.TrimPrefix(u.Path, "/"))
			b, err := assetFS.ReadFile(name)
			if err != nil {
				fulfill(404, "text/plain", nil)
				return
			}
			ct := mime.TypeByExtension(path.Ext(name))
			if ct == "" {
				ct = "application/octet-stream"
			}
			fulfill(200, ct, b)
		default:
			fulfill(404, "text/plain", nil)
		}
		return
	}

	// No navigating away (meta refresh, link clicks) and no subframes.
	if e.ResourceType == network.ResourceTypeDocument || !r.cfg.AllowRemote || !budget.take() {
		fail()
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		fail()
		return
	}
	ctx, cancel := context.WithTimeout(tab, 15*time.Second)
	defer cancel()
	res, err := r.fetcher.get(ctx, u.String(), budget)
	if err != nil {
		r.log.Debug("remote asset blocked", "url", u.Redacted(), "err", err)
		fail()
		return
	}
	// Cross-origin web fonts need CORS approval to be used by the page.
	fulfill(int64(res.status), res.contentType, res.body,
		&fetch.HeaderEntry{Name: "Access-Control-Allow-Origin", Value: "*"})
}

type inputError struct{ err error }

func (e *inputError) Error() string { return e.err.Error() }
func (e *inputError) Unwrap() error { return e.err }
