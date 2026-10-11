package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type dockerBox struct {
	bin   string
	image string
}

func newDocker(o Options) (*dockerBox, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("docker not found: %w", err)
	}
	return &dockerBox{bin: bin, image: o.DockerImage}, nil
}

func (d *dockerBox) Name() string { return "docker" }

func (d *dockerBox) Run(ctx context.Context, s Spec) (Result, error) {
	// The container runs as nobody; let it write into the bind-mounted dir.
	if err := os.Chmod(s.Dir, 0o777); err != nil {
		return Result{}, err
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "cs-run-" + hex.EncodeToString(b)
	l := s.Limits
	mem := strconv.Itoa(l.MemoryMB) + "m"
	args := []string{
		"run", "--rm", "-i", "--name", name,
		"--network", "none",
		"--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,size=64m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--user", "65534:65534",
		"--memory", mem, "--memory-swap", mem,
		"--pids-limit", strconv.Itoa(max(l.MaxProcs, 8)),
		"--cpus", "1",
		"--ulimit", "fsize=" + strconv.Itoa(max(l.FileSizeMB, 1)<<20),
		"--ulimit", "nofile=" + strconv.Itoa(max(l.OpenFiles, 64)),
		"--ulimit", "core=0",
		"-v", s.Dir + ":" + BoxDir + ":rw",
		"-w", BoxDir,
		"-e", "HOME=" + BoxDir,
		"-e", "TMPDIR=/tmp",
		"-e", "LANG=C.UTF-8",
	}
	for _, e := range s.Env {
		args = append(args, "-e", expand(e, BoxDir))
	}
	args = append(args, "--entrypoint", "", d.image)
	args = append(args, expandAll(s.Argv, BoxDir)...)

	cmd := exec.Command(d.bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	// Container start-up is slow; budget for it on top of the program limit.
	wall := l.WallTime + 3*time.Second
	stop := func(int) bool {
		_ = exec.Command(d.bin, "rm", "-f", name).Run()
		return false
	}
	return runProcess(ctx, cmd, wall, stop)
}
