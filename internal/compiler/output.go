package compiler

import (
	"sync"
	"time"
	"unicode/utf8"
)

// Chunk is a piece of program output destined for the client.
type Chunk struct {
	Stream string // "stdout" | "stderr"
	Phase  string // "compile" | "run"
	Data   string
}

// Sink receives batched output. It must not block for long; it is called
// from the coalescing goroutine.
type Sink func(Chunk)

// coalescer batches high-frequency small writes (e.g. a print in a tight
// loop) into ~25ms / 8KB frames, enforces the shared output budget, and never
// splits a UTF-8 sequence across frames.
type coalescer struct {
	mu        sync.Mutex
	sink      Sink
	phase     string
	pending   map[string][]byte
	order     []string
	budget    int
	exceeded  bool
	onExceed  func()
	flushTick *time.Ticker
	stop      chan struct{}
	stopped   chan struct{}
}

const (
	flushEvery = 25 * time.Millisecond
	flushBytes = 8 << 10
)

func newCoalescer(sink Sink, budget int, onExceed func()) *coalescer {
	c := &coalescer{
		sink:     sink,
		pending:  map[string][]byte{},
		budget:   budget,
		onExceed: onExceed,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	c.flushTick = time.NewTicker(flushEvery)
	go c.loop()
	return c
}

func (c *coalescer) loop() {
	defer close(c.stopped)
	for {
		select {
		case <-c.stop:
			return
		case <-c.flushTick.C:
			c.mu.Lock()
			c.flushLocked(false)
			c.mu.Unlock()
		}
	}
}

func (c *coalescer) setPhase(p string) {
	c.mu.Lock()
	c.flushLocked(true)
	c.phase = p
	c.mu.Unlock()
}

func (c *coalescer) writer(stream string) *streamWriter { return &streamWriter{c: c, stream: stream} }

func (c *coalescer) write(stream string, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.exceeded {
		return
	}
	if len(p) > c.budget {
		p = p[:c.budget]
		c.exceeded = true
	}
	c.budget -= len(p)
	if _, ok := c.pending[stream]; !ok {
		c.order = append(c.order, stream)
	}
	c.pending[stream] = append(c.pending[stream], p...)
	if len(c.pending[stream]) >= flushBytes {
		c.flushLocked(false)
	}
	if c.exceeded && c.onExceed != nil {
		c.flushLocked(true)
		go c.onExceed()
	}
}

// flushLocked emits pending output. Unless final, an incomplete trailing UTF-8
// sequence is held back until the rest of it arrives.
func (c *coalescer) flushLocked(final bool) {
	for _, s := range c.order {
		buf := c.pending[s]
		if len(buf) == 0 {
			continue
		}
		cut := len(buf)
		if !final {
			cut = utf8Boundary(buf)
		}
		if cut == 0 {
			continue
		}
		c.sink(Chunk{Stream: s, Phase: c.phase, Data: string(buf[:cut])})
		c.pending[s] = append(buf[:0:0], buf[cut:]...)
	}
}

func (c *coalescer) close() (exceeded bool) {
	close(c.stop)
	<-c.stopped
	c.flushTick.Stop()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked(true)
	return c.exceeded
}

// utf8Boundary returns the length of the longest prefix of b that does not end
// inside a (possibly still incomplete) multi-byte UTF-8 sequence.
func utf8Boundary(b []byte) int {
	n := len(b)
	for i := n - 1; i >= 0 && i >= n-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if r, size := utf8.DecodeRune(b[i:]); r == utf8.RuneError && size <= 1 && n-i < utf8.UTFMax && !utf8.FullRune(b[i:]) {
				return i
			}
			return n
		}
	}
	return n
}

type streamWriter struct {
	c      *coalescer
	stream string
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.c.write(w.stream, p)
	return len(p), nil
}
