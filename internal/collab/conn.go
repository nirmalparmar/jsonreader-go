package collab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	sendQueue     = 1024
	writeTimeout  = 10 * time.Second
	pingInterval  = 25 * time.Second
	joinTimeout   = 10 * time.Second
	msgRatePerSec = 60
	msgBurst      = 300
)

var sidRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// conn is one participant's WebSocket connection.
type conn struct {
	ws   *websocket.Conn
	room *Room
	ip   string

	sid   string // private session id (stable across reconnects of one tab)
	pid   string // public peer id
	name  string
	color string
	owner bool
	sel   *selection // guarded by room.mu

	send      chan []byte
	closeOnce sync.Once
	done      chan struct{}

	tokens float64
	last   time.Time
}

func newPID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (c *conn) info() peerInfo {
	return peerInfo{PID: c.pid, Name: c.name, Color: c.color, Owner: c.owner, Sel: c.sel.clone()}
}

// enqueue never blocks: a peer that cannot keep up is disconnected and will
// catch up from the op log when it reconnects.
func (c *conn) enqueue(b []byte) {
	select {
	case c.send <- b:
	case <-c.done:
	default:
		go c.close(websocket.StatusPolicyViolation, "connection too slow")
	}
}

func (c *conn) enqueueJSON(v any) {
	if b, err := json.Marshal(v); err == nil {
		c.enqueue(b)
	}
}

func (c *conn) sendError(code, msg string) {
	c.enqueueJSON(map[string]any{"t": "error", "code": code, "message": msg})
}

func (c *conn) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.ws.Close(code, reason)
	})
}

// allow is a per-connection token bucket guarding against message floods.
func (c *conn) allow() bool {
	now := time.Now()
	if c.last.IsZero() {
		c.tokens, c.last = msgBurst, now
	}
	c.tokens = min(msgBurst, c.tokens+now.Sub(c.last).Seconds()*msgRatePerSec)
	c.last = now
	if c.tokens < 1 {
		return false
	}
	c.tokens--
	return true
}

func (c *conn) writeLoop(ctx context.Context) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case b := <-c.send:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "write failed")
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Ping(pctx)
			cancel()
			if err != nil {
				c.close(websocket.StatusGoingAway, "ping timeout")
				return
			}
		}
	}
}

// serve runs the connection until it closes.
func (c *conn) serve(ctx context.Context, hub *Hub, roomID string) {
	defer c.close(websocket.StatusNormalClosure, "")
	go c.writeLoop(ctx)

	// The first message must be a join.
	jctx, cancel := context.WithTimeout(ctx, joinTimeout)
	var m inMsg
	err := c.readMsg(jctx, &m)
	cancel()
	if err != nil || m.T != "join" {
		c.close(websocket.StatusPolicyViolation, "expected join")
		return
	}
	if !sidRe.MatchString(m.SID) {
		c.close(websocket.StatusPolicyViolation, "invalid session id")
		return
	}
	c.sid = m.SID
	c.name = cleanText(m.Name, maxNameRunes, false)
	if c.name == "" {
		c.name = "Guest " + c.pid[:4]
	}
	room, err := hub.get(roomID)
	if err != nil {
		c.sendError("not_found", "this room does not exist or has expired")
		c.flushAndClose(websocket.StatusNormalClosure, "not found")
		return
	}
	rev := -1
	if m.Rev != nil {
		rev = *m.Rev
	}
	init, err := room.join(c, rev, m.OwnerToken)
	if err != nil {
		c.sendError("join_failed", err.Error())
		c.flushAndClose(websocket.StatusTryAgainLater, err.Error())
		return
	}
	c.room = room
	defer room.leave(c)
	c.enqueueJSON(init)

	for {
		var m inMsg
		if err := c.readMsg(ctx, &m); err != nil {
			return
		}
		if !c.allow() {
			c.close(websocket.StatusPolicyViolation, "rate limited")
			return
		}
		c.handle(hub, &m)
	}
}

func (c *conn) readMsg(ctx context.Context, m *inMsg) error {
	typ, b, err := c.ws.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("binary messages not supported")
	}
	return json.Unmarshal(b, m)
}

// flushAndClose gives the writer a moment to deliver a final error message.
func (c *conn) flushAndClose(code websocket.StatusCode, reason string) {
	deadline := time.Now().Add(time.Second)
	for len(c.send) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c.close(code, reason)
}

func (c *conn) handle(hub *Hub, m *inMsg) {
	r := c.room
	var err error
	switch m.T {
	case "op":
		if m.Rev == nil {
			err = &opError{code: "bad_op", msg: "missing rev"}
			break
		}
		err = r.applyOp(c, *m.Rev, m.Seq, m.Op, m.Sel)
	case "sel":
		r.setSelection(c, m.Sel)
	case "lang":
		err = r.setLang(c, m.Lang)
	case "name":
		r.setName(c, m.Name)
	case "chat":
		r.addChat(c, m.Text)
	case "lock":
		err = r.setLocked(c, m.Locked)
	case "run":
		if ok, _ := hub.runLimiter.Allow(c.ip); !ok {
			err = &opError{code: "rate_limited", msg: "too many runs, wait a moment"}
			break
		}
		err = r.startRun(c, m.Stdin)
	case "stop":
		err = r.stopRun(c)
	case "resync":
		r.resync(c)
	case "ping":
		c.enqueueJSON(map[string]any{"t": "pong"})
	default:
		err = &opError{code: "bad_message", msg: "unknown message type"}
	}
	var oe *opError
	if errors.As(err, &oe) {
		c.sendError(oe.code, oe.msg)
		if oe.resync {
			r.resync(c)
		}
	}
}
