package compiler

import (
	"strings"
	"testing"
	"time"
)

func TestDescribeDistinguishesCPUFromMemory(t *testing.T) {
	wall, cpu := 120*time.Second, 30*time.Second
	cases := []struct {
		name string
		r    Result
		want string
	}{
		{"wall timeout", Result{TimedOut: true, Signal: "SIGKILL", RuntimeMs: 120000}, "Time limit exceeded (2m0s)"},
		{"busy loop hits CPU budget", Result{Signal: "SIGKILL", RuntimeMs: 30050}, "CPU time limit exceeded (30s"},
		{"SIGXCPU", Result{Signal: "SIGXCPU", RuntimeMs: 30010}, "CPU time limit exceeded"},
		{"fast kill is memory", Result{Signal: "SIGKILL", RuntimeMs: 900}, "256 MB memory limit"},
		{"output cap", Result{Truncated: true}, "Output limit"},
		{"clean exit", Result{}, ""},
	}
	for _, c := range cases {
		got := describe(c.r, wall, cpu, 256, 1<<20)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
	}
}
