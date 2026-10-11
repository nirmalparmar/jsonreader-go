// Package config loads service configuration from environment variables.
// Every knob has a safe default so the service boots with zero configuration
// in development; production deployments override via env (see .env.example).
package config

import (
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr            string
	Env             string // "development" | "production"
	AllowedOrigins  []string
	RequireOrigin   bool // reject browser-less requests (no Origin header) on mutating endpoints
	TrustedProxies  []netip.Prefix
	ShutdownTimeout time.Duration
	DataDir         string

	Compiler CompilerConfig
	PDF      PDFConfig
	Collab   CollabConfig
}

type CompilerConfig struct {
	Enabled        bool
	Sandbox        string // nsjail | docker | unsafe
	NsjailPath     string
	DockerImage    string
	WorkRoot       string
	MaxConcurrent  int
	QueueTimeout   time.Duration
	MaxCodeBytes   int
	MaxStdinBytes  int
	MaxOutputBytes int
	RunTimeout     time.Duration
	CompileTimeout time.Duration
	MemoryMB       int
	MaxProcs       int
	RatePerMinute  int
	RateBurst      int
}

type PDFConfig struct {
	Enabled         bool
	ChromePath      string
	ChromeNoSandbox bool
	ChromeUID       int // when the server runs as root, run Chrome as this uid (0 = same user)
	MaxConcurrent   int
	MaxInputBytes   int
	RenderTimeout   time.Duration
	AllowRemote     bool // allow fetching public http(s) images/fonts/css referenced by documents
	MaxRemoteBytes  int
	RatePerMinute   int
	RateBurst       int
}

type CollabConfig struct {
	Enabled          bool
	MaxDocBytes      int
	MaxPeersPerRoom  int
	MaxRooms         int
	RoomTTL          time.Duration // delete persisted rooms untouched for this long
	IdleUnload       time.Duration // drop empty rooms from memory after this long
	CreatePerMinute  int
	ConnectPerMinute int
	RunsPerMinute    int
}

func Load() (*Config, error) {
	c := &Config{
		Addr:            env("ADDR", ":8080"),
		Env:             env("APP_ENV", "development"),
		AllowedOrigins:  list("ALLOWED_ORIGINS", "https://www.jsonreader.com,https://jsonreader.com,http://localhost:3000"),
		RequireOrigin:   boolean("REQUIRE_ORIGIN", false),
		ShutdownTimeout: duration("SHUTDOWN_TIMEOUT", 20*time.Second),
		DataDir:         env("DATA_DIR", "./data"),
		Compiler: CompilerConfig{
			Enabled:        boolean("COMPILER_ENABLED", true),
			Sandbox:        env("SANDBOX", "nsjail"),
			NsjailPath:     env("NSJAIL_PATH", "nsjail"),
			DockerImage:    env("SANDBOX_DOCKER_IMAGE", "common-service-runner:latest"),
			WorkRoot:       env("SANDBOX_WORK_ROOT", os.TempDir()),
			MaxConcurrent:  integer("COMPILER_MAX_CONCURRENT", max(2, runtime.NumCPU())),
			QueueTimeout:   duration("COMPILER_QUEUE_TIMEOUT", 10*time.Second),
			MaxCodeBytes:   integer("COMPILER_MAX_CODE_BYTES", 128<<10),
			MaxStdinBytes:  integer("COMPILER_MAX_STDIN_BYTES", 64<<10),
			MaxOutputBytes: integer("COMPILER_MAX_OUTPUT_BYTES", 1<<20),
			RunTimeout:     duration("COMPILER_RUN_TIMEOUT", 10*time.Second),
			CompileTimeout: duration("COMPILER_COMPILE_TIMEOUT", 30*time.Second),
			MemoryMB:       integer("COMPILER_MEMORY_MB", 256),
			MaxProcs:       integer("COMPILER_MAX_PROCS", 64),
			RatePerMinute:  integer("COMPILER_RATE_PER_MINUTE", 30),
			RateBurst:      integer("COMPILER_RATE_BURST", 10),
		},
		PDF: PDFConfig{
			Enabled:         boolean("PDF_ENABLED", false), // parked for now; see README
			ChromePath:      env("CHROME_PATH", ""),
			ChromeNoSandbox: boolean("CHROME_NO_SANDBOX", false),
			ChromeUID:       integer("CHROME_UID", 0),
			MaxConcurrent:   integer("PDF_MAX_CONCURRENT", max(2, runtime.NumCPU()/2)),
			MaxInputBytes:   integer("PDF_MAX_INPUT_BYTES", 2<<20),
			RenderTimeout:   duration("PDF_RENDER_TIMEOUT", 30*time.Second),
			AllowRemote:     boolean("PDF_ALLOW_REMOTE", true),
			MaxRemoteBytes:  integer("PDF_MAX_REMOTE_BYTES", 20<<20),
			RatePerMinute:   integer("PDF_RATE_PER_MINUTE", 12),
			RateBurst:       integer("PDF_RATE_BURST", 5),
		},
		Collab: CollabConfig{
			Enabled:          boolean("COLLAB_ENABLED", true),
			MaxDocBytes:      integer("COLLAB_MAX_DOC_BYTES", 1<<20),
			MaxPeersPerRoom:  integer("COLLAB_MAX_PEERS", 50),
			MaxRooms:         integer("COLLAB_MAX_ROOMS", 50000),
			RoomTTL:          duration("COLLAB_ROOM_TTL", 30*24*time.Hour),
			IdleUnload:       duration("COLLAB_IDLE_UNLOAD", 10*time.Minute),
			CreatePerMinute:  integer("COLLAB_CREATE_PER_MINUTE", 10),
			ConnectPerMinute: integer("COLLAB_CONNECT_PER_MINUTE", 60),
			RunsPerMinute:    integer("COLLAB_RUNS_PER_MINUTE", 20),
		},
	}

	for _, s := range list("TRUSTED_PROXIES", "127.0.0.1/32,::1/128") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
		}
		c.TrustedProxies = append(c.TrustedProxies, p)
	}
	switch c.Compiler.Sandbox {
	case "nsjail", "docker", "unsafe":
	default:
		return nil, fmt.Errorf("SANDBOX must be nsjail, docker or unsafe (got %q)", c.Compiler.Sandbox)
	}
	if c.Compiler.Sandbox == "unsafe" && c.IsProduction() {
		return nil, fmt.Errorf("SANDBOX=unsafe is refused when APP_ENV=production")
	}
	return c, nil
}

func (c *Config) IsProduction() bool { return c.Env == "production" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func list(key, def string) []string {
	var out []string
	for _, s := range strings.Split(env(key, def), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func integer(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil {
		return n
	}
	return def
}

func boolean(key string, def bool) bool {
	if b, err := strconv.ParseBool(env(key, "")); err == nil {
		return b
	}
	return def
}

func duration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(env(key, "")); err == nil {
		return d
	}
	return def
}
