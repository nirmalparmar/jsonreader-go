package sandbox

import (
	"testing"
	"time"
)

func TestCPUSeconds(t *testing.T) {
	cases := []struct {
		l    Limits
		want int
	}{
		{Limits{WallTime: 120 * time.Second, CPUTime: 30 * time.Second}, 30},
		{Limits{WallTime: 120 * time.Second}, 120},                          // unset -> wall
		{Limits{WallTime: 10 * time.Second, CPUTime: 30 * time.Second}, 10}, // never above wall
		{Limits{WallTime: 1500 * time.Millisecond}, 2},                      // rounds up
	}
	for _, c := range cases {
		if got := c.l.cpuSeconds(); got != c.want {
			t.Errorf("%+v: got %d, want %d", c.l, got, c.want)
		}
	}
}
