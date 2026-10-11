package mdpdf

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/nirmalparmar/common-service/internal/config"
	"github.com/nirmalparmar/common-service/internal/httpx"
)

type pdfRequest struct {
	Markdown string  `json:"markdown"`
	Filename string  `json:"filename"`
	Options  Options `json:"options"`
}

// Routes registers:
//
//	POST /v1/markdown/pdf      render Markdown to application/pdf
//	GET  /v1/markdown/options  themes, code themes and page sizes
func Routes(mux *http.ServeMux, r *Renderer, cfg config.PDFConfig, limit func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /v1/markdown/options", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		httpx.JSON(w, http.StatusOK, map[string]any{
			"themes":      Themes(),
			"code_themes": CodeThemes(),
			"page_sizes":  PageSizes(),
			"max_bytes":   cfg.MaxInputBytes,
		})
	})
	mux.Handle("POST /v1/markdown/pdf", limit(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body pdfRequest
		if !httpx.DecodeJSON(w, req, int64(cfg.MaxInputBytes+64<<10), &body) {
			return
		}
		if strings.TrimSpace(body.Markdown) == "" {
			httpx.Error(w, http.StatusBadRequest, "empty_markdown", "markdown is empty")
			return
		}
		if len(body.Markdown) > cfg.MaxInputBytes {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("markdown exceeds %d bytes", cfg.MaxInputBytes))
			return
		}
		if len(body.Options.CSS) > 256<<10 {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "too_large", "custom css exceeds 256 KB")
			return
		}

		res, err := r.Render(req.Context(), body.Markdown, body.Options)
		if err != nil {
			var ie *inputError
			switch {
			case errors.As(err, &ie):
				httpx.Error(w, http.StatusBadRequest, "invalid_options", ie.Error())
			case errors.Is(err, ErrBusy):
				w.Header().Set("Retry-After", "5")
				httpx.Error(w, http.StatusServiceUnavailable, "busy", err.Error())
			case req.Context().Err() != nil:
			default:
				r.log.Error("pdf render failed", "err", err, "req", httpx.RequestID(req))
				httpx.Error(w, http.StatusInternalServerError, "render_failed", "could not render PDF")
			}
			return
		}

		name := safeFilename(body.Filename, res.Title)
		h := w.Header()
		h.Set("Content-Type", "application/pdf")
		h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		h.Set("Content-Length", strconv.Itoa(len(res.PDF)))
		h.Set("Cache-Control", "no-store")
		if len(res.Warns) > 0 {
			h.Set("X-Render-Warnings", strconv.Itoa(len(res.Warns)))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(res.PDF)
	})))
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._ -]+`)

func safeFilename(name, title string) string {
	if name == "" {
		name = title
	}
	name = strings.TrimSuffix(strings.TrimSpace(name), ".pdf")
	name = strings.Trim(unsafeName.ReplaceAllString(name, "-"), "-. ")
	if name == "" {
		name = "document"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name + ".pdf"
}
