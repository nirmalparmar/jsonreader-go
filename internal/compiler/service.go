// Package compiler compiles and runs untrusted source code inside a sandbox
// and streams its output.
package compiler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nirmalparmar/common-service/internal/config"
	"github.com/nirmalparmar/common-service/internal/sandbox"
)

var ErrBusy = errors.New("all execution slots are busy, try again shortly")

type Service struct {
	cfg   config.CompilerConfig
	box   sandbox.Sandbox
	log   *slog.Logger
	slots chan struct{}

	mu      sync.RWMutex
	langs   map[string]*Language // id and aliases -> language
	ordered []*Language
}

type Result struct {
	Phase     string `json:"phase"` // phase that finished last: compile | run
	ExitCode  int    `json:"exit_code"`
	Signal    string `json:"signal,omitempty"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
	RuntimeMs int64  `json:"runtime_ms"`
	CompileMs int64  `json:"compile_ms,omitempty"`
	Note      string `json:"note,omitempty"`
}

func New(cfg config.CompilerConfig, box sandbox.Sandbox, log *slog.Logger) *Service {
	return &Service{
		cfg:   cfg,
		box:   box,
		log:   log,
		slots: make(chan struct{}, max(cfg.MaxConcurrent, 1)),
		langs: map[string]*Language{},
	}
}

// Probe detects which toolchains actually work inside the sandbox and
// enables only those languages.
func (s *Service) Probe(ctx context.Context) {
	all := builtinLanguages()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok []*Language
	for _, l := range all {
		wg.Add(1)
		go func(l *Language) {
			defer wg.Done()
			version, err := s.probe(ctx, l)
			if err != nil {
				s.log.Info("language unavailable", "lang", l.ID, "reason", err)
				return
			}
			l.Version = version
			mu.Lock()
			ok = append(ok, l)
			mu.Unlock()
		}(l)
	}
	wg.Wait()

	order := map[string]int{}
	for i, l := range all {
		order[l.ID] = i
	}
	sort.Slice(ok, func(i, j int) bool { return order[ok[i].ID] < order[ok[j].ID] })

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ordered = ok
	s.langs = map[string]*Language{}
	for _, l := range ok {
		s.langs[l.ID] = l
		for _, a := range l.Aliases {
			s.langs[a] = l
		}
		s.log.Info("language enabled", "lang", l.ID, "version", l.Version)
	}
}

func (s *Service) probe(ctx context.Context, l *Language) (string, error) {
	dir, cleanup, err := s.workdir()
	if err != nil {
		return "", err
	}
	defer cleanup()
	var out bytes.Buffer
	res, err := s.box.Run(ctx, sandbox.Spec{
		Dir: dir, Argv: l.probe, Env: l.env,
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out,
		Limits: sandbox.Limits{WallTime: 30 * time.Second, MemoryMB: max(l.memoryMB, l.compileMemoryMB, 512), MaxProcs: 128, FileSizeMB: 8, OpenFiles: 256},
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 || res.TimedOut {
		return "", fmt.Errorf("probe exited %d: %s", res.ExitCode, firstLine(out.String()))
	}
	return firstLine(out.String()), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func (s *Service) SandboxName() string { return s.box.Name() }

func (s *Service) Languages() []*Language {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ordered
}

func (s *Service) Lookup(id string) *Language {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.langs[strings.ToLower(strings.TrimSpace(id))]
}

func (s *Service) Limits() config.CompilerConfig { return s.cfg }

// Acquire reserves an execution slot, waiting up to the queue timeout.
func (s *Service) Acquire(ctx context.Context) (release func(), err error) {
	t := time.NewTimer(s.cfg.QueueTimeout)
	defer t.Stop()
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, nil
	case <-t.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Service) workdir() (dir string, cleanup func(), err error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	root := filepath.Join(s.cfg.WorkRoot, "cs-"+hex.EncodeToString(b))
	dir = filepath.Join(root, "box")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("create workdir: %w", err)
	}
	// MkdirAll applies umask; make the parent traversable for jail uids.
	_ = os.Chmod(root, 0o711)
	_ = os.Chmod(dir, 0o700)
	return dir, func() { _ = os.RemoveAll(root) }, nil
}

// Execute compiles (if needed) and runs code, streaming output to sink. The
// caller must hold a slot from Acquire.
func (s *Service) Execute(ctx context.Context, l *Language, code, stdin string, sink Sink) (Result, error) {
	dir, cleanup, err := s.workdir()
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	if err := os.WriteFile(filepath.Join(dir, l.File), []byte(code), 0o644); err != nil {
		return Result{}, fmt.Errorf("write source: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := newCoalescer(sink, s.cfg.MaxOutputBytes, cancel)

	memory := s.cfg.MemoryMB
	if l.memoryMB > 0 {
		memory = l.memoryMB
	}
	procs := s.cfg.MaxProcs
	if l.procs > 0 {
		procs = l.procs
	}

	var res Result
	if l.compile != nil {
		if l.seed != "" {
			// A private copy, never a hardlink: the jail owns its box and
			// must not be able to modify a cache shared with other runs.
			if err := copyTree(l.seed, filepath.Join(dir, ".cache", "go")); err != nil && !errors.Is(err, fs.ErrNotExist) {
				s.log.Warn("seed cache copy failed", "lang", l.ID, "err", err)
			}
		}
		out.setPhase("compile")
		cmem := max(memory, l.compileMemoryMB)
		r, err := s.box.Run(ctx, sandbox.Spec{
			Dir: dir, Argv: l.compile, Env: l.env,
			Stdin: strings.NewReader(""), Stdout: out.writer("stderr"), Stderr: out.writer("stderr"),
			Limits: sandbox.Limits{WallTime: s.cfg.CompileTimeout, MemoryMB: cmem, MaxProcs: procs, FileSizeMB: 64, OpenFiles: 512},
		})
		res.CompileMs = r.Duration.Milliseconds()
		if err != nil || r.ExitCode != 0 || r.TimedOut {
			res.Phase = "compile"
			res.ExitCode, res.Signal, res.TimedOut = r.ExitCode, r.Signal, r.TimedOut
			res.Truncated = out.close()
			res.Note = describe(res, s.cfg.CompileTimeout, cmem, s.cfg.MaxOutputBytes)
			if ctx.Err() != nil && !res.Truncated {
				return res, ctx.Err()
			}
			return res, err
		}
	}

	out.setPhase("run")
	r, err := s.box.Run(ctx, sandbox.Spec{
		Dir: dir, Argv: l.run, Env: l.env,
		Stdin: strings.NewReader(stdin), Stdout: out.writer("stdout"), Stderr: out.writer("stderr"),
		Limits: sandbox.Limits{WallTime: s.cfg.RunTimeout, MemoryMB: memory, MaxProcs: procs, FileSizeMB: 16, OpenFiles: 256},
	})
	res.Phase = "run"
	res.ExitCode, res.Signal, res.TimedOut = r.ExitCode, r.Signal, r.TimedOut
	res.RuntimeMs = r.Duration.Milliseconds()
	res.Truncated = out.close()
	res.Note = describe(res, s.cfg.RunTimeout, memory, s.cfg.MaxOutputBytes)
	if res.Truncated {
		// We cancelled it ourselves; that is not a client disconnect.
		return res, nil
	}
	return res, err
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}

func describe(r Result, timeout time.Duration, memMB, outBytes int) string {
	switch {
	case r.Truncated:
		return fmt.Sprintf("Output limit of %d KB exceeded; the program was stopped.", outBytes>>10)
	case r.TimedOut:
		return fmt.Sprintf("Time limit exceeded (%s); the program was stopped.", timeout)
	case r.Signal == "SIGKILL":
		return fmt.Sprintf("Killed — most likely exceeded the %d MB memory limit.", memMB)
	case r.Signal == "SIGXFSZ":
		return "File size limit exceeded."
	case r.Signal != "":
		return "Terminated by " + r.Signal + "."
	}
	return ""
}
