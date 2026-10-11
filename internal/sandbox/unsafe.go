package sandbox

import (
	"context"
	"os"
	"os/exec"
	"strconv"
)

// unsafeBox runs programs as ordinary child processes of the server with a
// few ulimits. It provides NO isolation: code can read files and use the
// network as the server user. Only for local development of the API itself.
type unsafeBox struct{}

func (unsafeBox) Name() string { return "unsafe (no isolation — development only)" }

func (unsafeBox) Run(ctx context.Context, s Spec) (Result, error) {
	l := s.Limits
	script := "ulimit -t " + strconv.Itoa(l.cpuSeconds()) +
		" -f " + strconv.Itoa(max(l.FileSizeMB, 1)*2048) + // 512-byte blocks
		" -n " + strconv.Itoa(max(l.OpenFiles, 64)) +
		` 2>/dev/null; exec "$@"`
	argv := append([]string{"-c", script, "sh"}, expandAll(s.Argv, s.Dir)...)
	cmd := exec.Command("/bin/sh", argv...)
	cmd.Dir = s.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + s.Dir,
		"TMPDIR=" + s.Dir,
		"LANG=C.UTF-8",
	}, expandAll(s.Env, s.Dir)...)
	return runProcess(ctx, cmd, l.WallTime, nil)
}
