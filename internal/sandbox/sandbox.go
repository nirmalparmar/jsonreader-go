// Package sandbox executes untrusted programs with strict isolation and
// resource limits. Backends:
//
//   - nsjail: production. Linux namespaces (no network, private mount tree
//     with read-only toolchains), cgroup v2 memory/pids/cpu limits, rlimits
//     and a seccomp deny-list. Startup overhead is a few milliseconds.
//   - docker: one throwaway container per execution (--network none,
//     read-only rootfs, all caps dropped). Slower, but works anywhere Docker
//     does, including macOS.
//   - unsafe: plain child process with ulimits. Development only; refused in
//     production by config validation.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// BoxDir is where the per-execution work directory is mounted inside the
// jail. Argv and Env may reference it via the "{box}" placeholder.
const BoxDir = "/box"

type Limits struct {
	WallTime   time.Duration
	MemoryMB   int
	MaxProcs   int
	FileSizeMB int
	OpenFiles  int
}

type Spec struct {
	Dir    string // host directory, mounted read-write at BoxDir
	Argv   []string
	Env    []string // KEY=VALUE; "{box}" expands to the in-jail work dir
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Limits Limits
}

type Result struct {
	ExitCode int
	Signal   string
	TimedOut bool
	Duration time.Duration
}

type Sandbox interface {
	Name() string
	Run(ctx context.Context, spec Spec) (Result, error)
}

// Options configures backend construction.
type Options struct {
	Backend     string
	NsjailPath  string
	DockerImage string
	Slots       int // max concurrent executions (sizes the uid pool)
}

func New(o Options) (Sandbox, error) {
	switch o.Backend {
	case "nsjail":
		return newNsjail(o)
	case "docker":
		return newDocker(o)
	case "unsafe":
		return &unsafeBox{}, nil
	}
	return nil, fmt.Errorf("unknown sandbox backend %q", o.Backend)
}

func expand(s, box string) string { return strings.ReplaceAll(s, "{box}", box) }

func expandAll(in []string, box string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = expand(s, box)
	}
	return out
}

// runProcess starts cmd in its own process group and enforces the wall-clock
// limit. On timeout or cancellation it calls stop (if any): returning true
// means a graceful shutdown was requested, e.g. SIGTERM to nsjail so it can
// tear down its jail and cgroup; the group is SIGKILLed if that does not
// finish promptly, or immediately when stop returns false.
func runProcess(ctx context.Context, cmd *exec.Cmd, wall time.Duration, stop func(pid int) bool) (Result, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start sandbox: %w", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	timer := time.NewTimer(wall)
	defer timer.Stop()

	var res Result
	var err error
	terminate := func() {
		if stop != nil && stop(cmd.Process.Pid) {
			grace := time.NewTimer(1500 * time.Millisecond)
			defer grace.Stop()
			select {
			case err = <-waitErr:
				return
			case <-grace.C:
			}
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-waitErr
	}
	select {
	case err = <-waitErr:
	case <-timer.C:
		res.TimedOut = true
		terminate()
	case <-ctx.Done():
		terminate()
	}
	// Reap any stragglers left in the group (e.g. orphaned background jobs
	// in unsafe mode).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	res.Duration = time.Since(start)

	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = signalName(ws.Signal())
			res.ExitCode = 128 + int(ws.Signal())
		} else {
			res.ExitCode = exitErr.ExitCode()
		}
	default:
		return res, err
	}
	// nsjail and docker report a killed child as 128+signal.
	if res.Signal == "" && res.ExitCode > 128 && res.ExitCode < 128+65 {
		res.Signal = signalName(syscall.Signal(res.ExitCode - 128))
	}
	if res.TimedOut {
		res.ExitCode, res.Signal = 128+int(syscall.SIGKILL), "SIGKILL"
	}
	if ctx.Err() != nil && !res.TimedOut {
		return res, ctx.Err()
	}
	return res, nil
}

func signalName(s syscall.Signal) string {
	switch s {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGFPE:
		return "SIGFPE"
	case syscall.SIGBUS:
		return "SIGBUS"
	case syscall.SIGXCPU:
		return "SIGXCPU"
	case syscall.SIGXFSZ:
		return "SIGXFSZ"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGILL:
		return "SIGILL"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	}
	return fmt.Sprintf("signal %d", int(s))
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }
