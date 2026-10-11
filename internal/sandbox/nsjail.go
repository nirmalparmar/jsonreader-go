package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// seccompPolicy (kafel syntax) denies syscalls that untrusted code has no
// business making and that widen kernel attack surface. Namespaces and the
// unprivileged uid already neutralise most of them; this is defence in depth.
// clone3 (syscall 435 on both amd64 and arm64; kafel has no name for it)
// returns ENOSYS so libc falls back to clone(), whose flags we can
// inspect to forbid creating nested user namespaces.
const seccompPolicy = `
ERRNO(1) {
	ptrace, process_vm_readv, process_vm_writev, kexec_load, bpf, userfaultfd,
	perf_event_open, mount, pivot_root, swapon, swapoff, reboot,
	init_module, finit_module, delete_module, add_key, request_key, keyctl,
	setns, unshare, acct, open_by_handle_at, quotactl, syslog,
	clone(flags) { (flags & 0x10000000) != 0 }
}
ERRNO(38) { SYSCALL[435] }
DEFAULT ALLOW
`

// roPaths are host paths exposed read-only inside the jail when present:
// the toolchains and the minimum of /etc that runtimes need.
var roPaths = []string{
	"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin", "/opt/runtimes",
	"/etc/alternatives", "/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/localtime", "/etc/passwd", "/etc/group", "/etc/nsswitch.conf",
	"/etc/ssl/certs", "/etc/ca-certificates", "/etc/mime.types",
}

const jailPATH = "/usr/local/bin:/usr/bin:/bin:/opt/runtimes/bin"

func lookPath(name string) (string, error) {
	for _, dir := range strings.Split(jailPATH, ":") {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: not found in sandbox PATH", name)
}

var devices = []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom"}

type nsjail struct {
	bin     string
	mounts  []string
	cgroups bool
	uids    chan int
}

func newNsjail(o Options) (*nsjail, error) {
	bin, err := exec.LookPath(o.NsjailPath)
	if err != nil {
		return nil, fmt.Errorf("nsjail not found (%s): %w", o.NsjailPath, err)
	}
	n := &nsjail{bin: bin, cgroups: cgroupV2Writable()}
	if n.cgroups {
		go sweepCgroups()
	}
	for _, p := range roPaths {
		if exists(p) {
			n.mounts = append(n.mounts, p)
		}
	}
	// JVM configuration lives in /etc/java-<ver>-openjdk.
	if m, _ := filepath.Glob("/etc/java-*"); len(m) > 0 {
		n.mounts = append(n.mounts, m...)
	}
	// Every concurrent execution gets its own unprivileged host uid so jails
	// cannot signal, ptrace or share rlimit accounting with each other.
	slots := max(o.Slots, 1)
	n.uids = make(chan int, slots)
	for i := range slots {
		n.uids <- 61000 + i
	}
	return n, nil
}

func (n *nsjail) Name() string {
	if n.cgroups {
		return "nsjail+cgroupv2"
	}
	return "nsjail"
}

// cgroupV2Writable reports whether nsjail can create child cgroups for
// memory/pids/cpu accounting (requires the entrypoint to have enabled the
// controllers; see deploy/entrypoint.sh).
func cgroupV2Writable() bool {
	b, err := os.ReadFile("/sys/fs/cgroup/cgroup.subtree_control")
	if err != nil {
		return false
	}
	ctl := string(b)
	return strings.Contains(ctl, "memory") && strings.Contains(ctl, "pids")
}

func (n *nsjail) Run(ctx context.Context, s Spec) (Result, error) {
	var uid int
	select {
	case uid = <-n.uids:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { n.uids <- uid }()

	// Hand the work dir to the jail uid; nsjail maps it 1:1 inside.
	if err := chownTree(s.Dir, uid); err != nil {
		return Result{}, err
	}
	defer chownTree(s.Dir, os.Getuid()) //nolint:errcheck // best effort, dir is removed next

	l := s.Limits
	wallSec := int(l.WallTime.Seconds()) + 2 // our own timer fires first; this is a backstop
	id := strconv.Itoa(uid)
	logPath := filepath.Join(filepath.Dir(s.Dir), "nsjail.log")
	args := []string{
		"--mode", "o",
		"--quiet",
		"--log", logPath,
		"--hostname", "sandbox",
		"--cwd", BoxDir,
		"--user", id + ":" + id + ":1",
		"--group", id + ":" + id + ":1",
		"--time_limit", strconv.Itoa(wallSec),
		"--rlimit_cpu", strconv.Itoa(wallSec),
		"--rlimit_fsize", strconv.Itoa(max(l.FileSizeMB, 1)),
		"--rlimit_nofile", strconv.Itoa(max(l.OpenFiles, 64)),
		"--rlimit_stack", "64",
		"--rlimit_core", "0",
		"--seccomp_string", seccompPolicy,
		"--mount", "none:/tmp:tmpfs:size=67108864",
		"--bindmount", s.Dir + ":" + BoxDir,
		"--env", "PATH=" + jailPATH,
		"--env", "HOME=" + BoxDir,
		"--env", "TMPDIR=/tmp",
		"--env", "LANG=C.UTF-8",
	}
	if n.cgroups {
		args = append(args,
			"--use_cgroupv2", "--cgroupv2_mount", "/sys/fs/cgroup",
			"--cgroup_mem_max", strconv.Itoa(l.MemoryMB<<20),
			"--cgroup_mem_swap_max", "0",
			"--cgroup_pids_max", strconv.Itoa(max(l.MaxProcs, 8)),
			"--cgroup_cpu_ms_per_sec", "1000",
			"--rlimit_as", "inf",
		)
	} else {
		// Without cgroups fall back to address-space limits. Runtimes that
		// reserve large virtual ranges (JVM, V8) need generous headroom.
		args = append(args,
			"--rlimit_as", strconv.Itoa(max(l.MemoryMB*4, 1024)),
			"--rlimit_nproc", strconv.Itoa(max(l.MaxProcs, 8)),
		)
	}
	for _, m := range n.mounts {
		args = append(args, "--bindmount_ro", m)
	}
	for _, d := range devices {
		if exists(d) {
			args = append(args, "--bindmount", d)
		}
	}
	for _, e := range s.Env {
		args = append(args, "--env", expand(e, BoxDir))
	}
	argv := expandAll(s.Argv, BoxDir)
	if len(argv) == 0 {
		return Result{}, fmt.Errorf("empty argv")
	}
	// nsjail execve()s argv[0] without a PATH search. The jail mounts the
	// same read-only toolchain paths, so resolving on the host is exact.
	if !strings.Contains(argv[0], "/") {
		p, err := lookPath(argv[0])
		if err != nil {
			return Result{}, err
		}
		argv[0] = p
	}
	args = append(args, "--")
	args = append(args, argv...)

	cmd := exec.Command(n.bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	cmd.Env = []string{}
	// SIGTERM lets nsjail kill the jail and remove its cgroup itself.
	stop := func(pid int) bool { return syscall.Kill(pid, syscall.SIGTERM) == nil }
	res, err := runProcess(ctx, cmd, l.WallTime, stop)
	if err == nil && !res.TimedOut && res.ExitCode == 255 {
		// 255 is nsjail's own failure code; surface why the jail did not start.
		if msg := jailFailure(logPath); msg != "" {
			return res, fmt.Errorf("nsjail: %s", msg)
		}
	}
	return res, err
}

// jailFailure extracts error/fatal lines from an nsjail log, if any.
func jailFailure(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "[E]") || strings.HasPrefix(l, "[F]") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, " | ")
}

// sweepCgroups removes empty per-jail cgroups that nsjail could not clean up
// (e.g. if it was SIGKILLed). rmdir only succeeds on cgroups with no tasks.
func sweepCgroups() {
	for range time.Tick(time.Minute) {
		dirs, _ := filepath.Glob("/sys/fs/cgroup/NSJAIL.*")
		for _, d := range dirs {
			_ = syscall.Rmdir(d)
		}
	}
}

func chownTree(dir string, uid int) error {
	if os.Getuid() != 0 {
		return nil // only root can hand files to another uid; dev setups run unprivileged
	}
	return filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, uid)
	})
}
