package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func Error(w http.ResponseWriter, status int, code, msg string) {
	JSON(w, status, errorBody{Error: code, Message: msg})
}

// DecodeJSON reads a size-capped JSON body into dst, rejecting unknown
// trailing data. It writes the error response itself and returns false on
// failure.
func DecodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		Error(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			Error(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("request body exceeds %d bytes", maxBytes))
		} else {
			Error(w, http.StatusBadRequest, "bad_json", "invalid JSON body")
		}
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		Error(w, http.StatusBadRequest, "bad_json", "unexpected data after JSON body")
		return false
	}
	return true
}

// SSE is a server-sent-events writer. It is safe for concurrent use and keeps
// the connection alive with comment heartbeats until Close.
type SSE struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	rc     *http.ResponseController
	closed bool
	done   chan struct{}
}

// StartSSE switches the response into event-stream mode. The server-wide
// WriteTimeout is lifted for this response since streams outlive it.
func StartSSE(w http.ResponseWriter, heartbeat time.Duration) *SSE {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx proxy buffering
	w.WriteHeader(http.StatusOK)

	s := &SSE{w: w, rc: http.NewResponseController(w), done: make(chan struct{})}
	_ = s.rc.SetWriteDeadline(time.Time{})
	// Opening comment flushes headers through proxies. It is spelled
	// "heartbeat" because older clients ignore exactly that comment.
	_, _ = io.WriteString(w, ": heartbeat\n\n")
	_ = s.rc.Flush()
	if heartbeat > 0 {
		go s.heartbeat(heartbeat)
	}
	return s
}

func (s *SSE) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.mu.Lock()
			if !s.closed {
				_, _ = io.WriteString(s.w, ": heartbeat\n\n")
				_ = s.rc.Flush()
			}
			s.mu.Unlock()
		}
	}
}

// Send writes one event whose data is v encoded as single-line JSON.
func (s *SSE) Send(event string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return io.ErrClosedPipe
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	return s.rc.Flush()
}

func (s *SSE) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
}
