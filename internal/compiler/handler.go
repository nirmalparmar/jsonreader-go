package compiler

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/nirmalparmar/common-service/internal/httpx"
)

type runRequest struct {
	Language string `json:"language"`
	Code     string `json:"code"`
	Stdin    string `json:"stdin"`
}

// Event payloads. Field names (text / stderr / exit_code / runtime_ms) match
// what the jsonreader.com compiler pages already parse.
type (
	evStart struct {
		Language string `json:"language"`
		Version  string `json:"version"`
	}
	evStdout struct {
		Text  string `json:"text"`
		Phase string `json:"phase"`
	}
	evStderr struct {
		Stderr string `json:"stderr"`
		Phase  string `json:"phase"`
	}
	evError struct {
		Error  string `json:"error"`
		Stderr string `json:"stderr"` // human-readable copy for simple clients
	}
)

// Routes registers the compiler endpoints.
//
//	GET  /v1/languages           enabled languages + limits
//	POST /v1/run/{lang}          SSE stream (lang may also come from the body)
//	POST /v1/{lang}/stream       legacy alias used by existing clients
func (s *Service) Routes(mux *http.ServeMux, limit func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /v1/languages", s.handleLanguages)
	run := limit(http.HandlerFunc(s.handleRun))
	mux.Handle("POST /v1/run/{lang}", run)
	mux.Handle("POST /v1/run", run)
	// Legacy paths are registered literally: a "/v1/{lang}/stream" wildcard
	// would overlap ambiguously with "/v1/run/{lang}".
	seen := map[string]bool{}
	for _, l := range builtinLanguages() {
		for _, name := range append([]string{l.ID}, l.Aliases...) {
			if !seen[name] {
				seen[name] = true
				mux.Handle("POST /v1/"+name+"/stream", withLang(name, run))
			}
		}
	}
}

func withLang(lang string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("lang", lang)
		next.ServeHTTP(w, r)
	})
}

func (s *Service) handleLanguages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.JSON(w, http.StatusOK, map[string]any{
		"sandbox":   s.SandboxName(),
		"languages": s.Languages(),
		"limits": map[string]any{
			"run_timeout_ms":     s.cfg.RunTimeout.Milliseconds(),
			"compile_timeout_ms": s.cfg.CompileTimeout.Milliseconds(),
			"memory_mb":          s.cfg.MemoryMB,
			"max_code_bytes":     s.cfg.MaxCodeBytes,
			"max_stdin_bytes":    s.cfg.MaxStdinBytes,
			"max_output_bytes":   s.cfg.MaxOutputBytes,
		},
	})
}

func (s *Service) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if !httpx.DecodeJSON(w, r, int64(s.cfg.MaxCodeBytes+s.cfg.MaxStdinBytes+4096), &req) {
		return
	}
	id := r.PathValue("lang")
	if id == "" {
		id = req.Language
	}
	lang := s.Lookup(id)
	switch {
	case lang == nil:
		httpx.Error(w, http.StatusBadRequest, "unsupported_language", fmt.Sprintf("language %q is not available", id))
		return
	case len(req.Code) == 0:
		httpx.Error(w, http.StatusBadRequest, "empty_code", "code is empty")
		return
	case len(req.Code) > s.cfg.MaxCodeBytes:
		httpx.Error(w, http.StatusRequestEntityTooLarge, "code_too_large", fmt.Sprintf("code exceeds %d bytes", s.cfg.MaxCodeBytes))
		return
	case len(req.Stdin) > s.cfg.MaxStdinBytes:
		httpx.Error(w, http.StatusRequestEntityTooLarge, "stdin_too_large", fmt.Sprintf("stdin exceeds %d bytes", s.cfg.MaxStdinBytes))
		return
	}

	release, err := s.Acquire(r.Context())
	if err != nil {
		if errors.Is(err, ErrBusy) {
			w.Header().Set("Retry-After", "3")
			httpx.Error(w, http.StatusServiceUnavailable, "busy", err.Error())
		}
		return
	}
	defer release()

	sse := httpx.StartSSE(w, 15*time.Second)
	defer sse.Close()
	_ = sse.Send("start", evStart{Language: lang.ID, Version: lang.Version})

	res, err := s.Execute(r.Context(), lang, req.Code, req.Stdin, func(c Chunk) {
		if c.Stream == "stdout" {
			_ = sse.Send("stdout", evStdout{Text: c.Data, Phase: c.Phase})
		} else {
			_ = sse.Send("stderr", evStderr{Stderr: c.Data, Phase: c.Phase})
		}
	})
	if r.Context().Err() != nil {
		return // client went away; the sandbox has already been killed
	}
	if err != nil {
		s.log.Error("execute failed", "lang", lang.ID, "err", err, "req", httpx.RequestID(r))
		_ = sse.Send("error", evError{Error: "internal", Stderr: "\nInternal error while running your code. Please try again."})
		return
	}
	if res.Note != "" {
		_ = sse.Send("stderr", evStderr{Stderr: "\n[" + res.Note + "]\n", Phase: res.Phase})
	}
	_ = sse.Send("exit", res)
}
