package collab

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/nirmalparmar/common-service/internal/compiler"
)

const (
	maxLog       = 2000 // ops kept for reconnect catch-up
	maxChat      = 100
	maxChatRunes = 1000
	maxOutput    = 64 << 10
	maxSessions  = 500
	maxNameRunes = 32
	defaultLang  = "javascript"
)

type logEntry struct {
	op  *Operation
	sid string
	pid string
	seq int64
}

// Room is one shared document. All state is guarded by mu; outgoing
// messages are queued to peers while holding it so every peer observes the
// same total order of operations.
type Room struct {
	id  string
	hub *Hub

	mu        sync.Mutex
	doc       []uint16
	rev       int
	logBase   int // revision before log[0]
	log       []logEntry
	lastSeq   map[string]int64 // session id -> last applied client seq
	lang      string
	ownerHash string
	locked    bool
	chat      []chatMsg
	created   time.Time
	updated   time.Time
	dirty     bool
	peers     map[*conn]struct{}
	idleSince time.Time
	closed    bool

	run       *roomRun
	output    []outChunk
	outputLen int
}

type roomRun struct {
	cancel context.CancelFunc
	by     string
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

func (r *Room) isOwnerToken(tok string) bool {
	if tok == "" || r.ownerHash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(r.ownerHash)) == 1
}

func (r *Room) docString() string { return string(utf16.Decode(r.doc)) }

func (r *Room) peerList(except *conn) []peerInfo {
	out := make([]peerInfo, 0, len(r.peers))
	for c := range r.peers {
		if c != except {
			out = append(out, c.info())
		}
	}
	return out
}

// broadcastLocked queues msg to every peer except skip. Caller holds r.mu.
func (r *Room) broadcastLocked(msg any, skip *conn) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	for c := range r.peers {
		if c != skip {
			c.enqueue(b)
		}
	}
}

func (r *Room) broadcast(msg any, skip *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broadcastLocked(msg, skip)
}

func (r *Room) touchLocked() {
	r.updated = time.Now()
	r.dirty = true
}

var (
	errRoomFull   = errors.New("room is full")
	errRoomClosed = errors.New("room closed")
)

// join registers c and builds its init message. If the client already holds
// the document at revision rev (reconnect), it receives only the ops it
// missed so its unacknowledged local edits survive.
func (r *Room) join(c *conn, rev int, ownerToken string) (*initMsg, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errRoomClosed
	}
	if len(r.peers) >= r.hub.cfg.MaxPeersPerRoom {
		return nil, errRoomFull
	}
	c.owner = r.isOwnerToken(ownerToken)
	c.color = r.pickColorLocked()
	r.peers[c] = struct{}{}
	r.idleSince = time.Time{}

	m := r.initLocked(c, rev)
	r.broadcastLocked(map[string]any{"t": "join", "peer": c.info()}, c)
	return m, nil
}

func (r *Room) initLocked(c *conn, rev int) *initMsg {
	m := &initMsg{
		T: "init", Room: r.id, You: c.info(), Rev: r.rev, LastSeq: r.lastSeq[c.sid],
		Lang: r.lang, Locked: r.locked, Peers: r.peerList(c), Chat: r.chat,
		Languages: r.hub.languages(), Running: r.run != nil, Output: r.output,
		Limits: map[string]int{"max_doc_units": r.hub.cfg.MaxDocBytes / 2},
	}
	if rev >= r.logBase && rev <= r.rev {
		for i := rev - r.logBase; i < len(r.log); i++ {
			e := r.log[i]
			m.Catchup = append(m.Catchup, catchupEntry{Rev: r.logBase + i + 1, Op: e.op, Own: e.sid == c.sid, PID: e.pid})
		}
		if m.Catchup == nil {
			m.Catchup = []catchupEntry{}
		}
	} else {
		d := r.docString()
		m.Doc = &d
	}
	if m.Chat == nil {
		m.Chat = []chatMsg{}
	}
	return m
}

func (r *Room) pickColorLocked() string {
	used := map[string]int{}
	for c := range r.peers {
		used[c.color]++
	}
	best, bestN := palette[0], 1<<30
	for _, col := range palette {
		if used[col] < bestN {
			best, bestN = col, used[col]
		}
	}
	return best
}

func (r *Room) leave(c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.peers[c]; !ok {
		return
	}
	delete(r.peers, c)
	if len(r.peers) == 0 {
		r.idleSince = time.Now()
	}
	r.broadcastLocked(map[string]any{"t": "leave", "pid": c.pid}, nil)
}

type opError struct {
	code   string
	msg    string
	resync bool
}

func (e *opError) Error() string { return e.msg }

// applyOp integrates a client operation made against revision rev.
func (r *Room) applyOp(c *conn, rev int, seq int64, raw json.RawMessage, rawSel json.RawMessage) error {
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return &opError{code: "bad_op", msg: err.Error(), resync: true}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked && !c.owner {
		return &opError{code: "locked", msg: "the room owner has made this room read-only", resync: true}
	}
	if seq <= r.lastSeq[c.sid] {
		return nil // duplicate (re-sent after reconnect); already applied
	}
	if rev < r.logBase || rev > r.rev {
		return &opError{code: "stale", msg: "revision too old", resync: true}
	}
	cur := &op
	for i := rev - r.logBase; i < len(r.log); i++ {
		a, _, err := Transform(cur, r.log[i].op)
		if err != nil {
			return &opError{code: "bad_op", msg: err.Error(), resync: true}
		}
		cur = a
	}
	if cur.BaseLen() != len(r.doc) {
		return &opError{code: "bad_op", msg: "operation does not match document", resync: true}
	}
	if cur.TargetLen() > r.hub.cfg.MaxDocBytes/2 {
		return &opError{code: "too_large", msg: "document size limit reached", resync: true}
	}
	doc, err := cur.Apply(r.doc)
	if err != nil {
		return &opError{code: "bad_op", msg: err.Error(), resync: true}
	}
	r.doc = doc
	r.rev++
	r.log = append(r.log, logEntry{op: cur, sid: c.sid, pid: c.pid, seq: seq})
	if len(r.log) > maxLog {
		drop := len(r.log) - maxLog
		r.log = append([]logEntry(nil), r.log[drop:]...)
		r.logBase += drop
	}
	r.lastSeq[c.sid] = seq
	if len(r.lastSeq) > maxSessions {
		r.pruneSessionsLocked()
	}
	r.touchLocked()

	// Keep everyone's stored cursor in step with the new document.
	for p := range r.peers {
		if p != c {
			p.sel.transform(cur)
		}
	}
	if sel, err := parseSelection(rawSel, len(r.doc)); err == nil && sel != nil {
		c.sel = sel
	} else {
		c.sel.transform(cur)
	}

	c.enqueueJSON(map[string]any{"t": "ack", "rev": r.rev})
	r.broadcastLocked(map[string]any{"t": "op", "rev": r.rev, "op": cur, "pid": c.pid, "sel": c.sel}, c)
	return nil
}

// pruneSessionsLocked drops dedup state for sessions that are not connected.
func (r *Room) pruneSessionsLocked() {
	live := map[string]bool{}
	for c := range r.peers {
		live[c.sid] = true
	}
	for _, e := range r.log {
		live[e.sid] = true
	}
	for sid := range r.lastSeq {
		if !live[sid] {
			delete(r.lastSeq, sid)
		}
	}
}

func (r *Room) setSelection(c *conn, raw json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sel, err := parseSelection(raw, len(r.doc))
	if err != nil {
		return
	}
	c.sel = sel
	r.broadcastLocked(map[string]any{"t": "sel", "pid": c.pid, "sel": sel}, c)
}

func (r *Room) setLang(c *conn, lang string) error {
	lang = strings.ToLower(strings.TrimSpace(lang))
	if lang == "" || len(lang) > 32 || !isMonacoLang(lang) {
		return &opError{code: "bad_lang", msg: "unknown language"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked && !c.owner {
		return &opError{code: "locked", msg: "the room owner has made this room read-only"}
	}
	if r.lang == lang {
		return nil
	}
	r.lang = lang
	r.touchLocked()
	r.broadcastLocked(map[string]any{"t": "lang", "lang": lang, "pid": c.pid}, nil)
	return nil
}

func (r *Room) setName(c *conn, name string) {
	name = cleanText(name, maxNameRunes, false)
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	c.name = name
	r.broadcastLocked(map[string]any{"t": "peer", "peer": c.info()}, nil)
}

func (r *Room) addChat(c *conn, text string) {
	text = cleanText(text, maxChatRunes, true)
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	m := chatMsg{PID: c.pid, Name: c.name, Color: c.color, Text: text, TS: time.Now().UTC()}
	r.chat = append(r.chat, m)
	if len(r.chat) > maxChat {
		r.chat = append([]chatMsg(nil), r.chat[len(r.chat)-maxChat:]...)
	}
	r.touchLocked()
	r.broadcastLocked(map[string]any{"t": "chat", "msg": m}, nil)
}

func (r *Room) setLocked(c *conn, locked bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !c.owner {
		return &opError{code: "forbidden", msg: "only the room owner can change this"}
	}
	if r.locked == locked {
		return nil
	}
	r.locked = locked
	r.touchLocked()
	r.broadcastLocked(map[string]any{"t": "lock", "locked": locked, "pid": c.pid}, nil)
	return nil
}

// startRun executes the current document with the room language and streams
// output to every participant.
func (r *Room) startRun(c *conn, stdin string) error {
	comp := r.hub.comp
	if comp == nil {
		return &opError{code: "run_unavailable", msg: "code execution is disabled on this server"}
	}
	r.mu.Lock()
	if r.locked && !c.owner {
		r.mu.Unlock()
		return &opError{code: "locked", msg: "the room owner has made this room read-only"}
	}
	if r.run != nil {
		r.mu.Unlock()
		return &opError{code: "busy", msg: "code is already running in this room"}
	}
	lang := comp.Lookup(r.lang)
	if lang == nil {
		r.mu.Unlock()
		return &opError{code: "run_unavailable", msg: "running " + r.lang + " is not supported"}
	}
	limits := comp.Limits()
	code := r.docString()
	if strings.TrimSpace(code) == "" {
		r.mu.Unlock()
		return &opError{code: "empty_code", msg: "nothing to run"}
	}
	if len(code) > limits.MaxCodeBytes || len(stdin) > limits.MaxStdinBytes {
		r.mu.Unlock()
		return &opError{code: "too_large", msg: "code or input is too large to run"}
	}
	ctx, cancel := context.WithCancel(r.hub.ctx)
	r.run = &roomRun{cancel: cancel, by: c.pid}
	r.output, r.outputLen = nil, 0
	r.broadcastLocked(map[string]any{"t": "run_start", "pid": c.pid, "name": c.name, "lang": lang.ID, "version": lang.Version}, nil)
	r.mu.Unlock()

	go func() {
		defer cancel()
		var res compiler.Result
		release, err := comp.Acquire(ctx)
		if err == nil {
			res, err = comp.Execute(ctx, lang, code, stdin, func(ch compiler.Chunk) {
				r.appendOutput(outChunk{Stream: ch.Stream, Phase: ch.Phase, Data: ch.Data})
			})
			release()
		}
		end := map[string]any{"t": "run_end", "result": res}
		switch {
		case errors.Is(err, compiler.ErrBusy):
			end["error"] = "The server is busy; try again in a moment."
		case ctx.Err() != nil:
			end["error"] = "Stopped."
		case err != nil:
			r.hub.log.Error("room run failed", "room", r.id, "err", err)
			end["error"] = "Internal error while running the code."
		case res.Note != "":
			r.appendOutput(outChunk{Stream: "stderr", Phase: res.Phase, Data: "\n[" + res.Note + "]\n"})
		}
		r.mu.Lock()
		r.run = nil
		r.broadcastLocked(end, nil)
		r.mu.Unlock()
	}()
	return nil
}

func (r *Room) appendOutput(o outChunk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outputLen < maxOutput {
		r.output = append(r.output, o)
		r.outputLen += len(o.Data)
	}
	r.broadcastLocked(map[string]any{"t": "run_out", "stream": o.Stream, "phase": o.Phase, "data": o.Data}, nil)
}

func (r *Room) stopRun(c *conn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run == nil {
		return nil
	}
	if r.locked && !c.owner && r.run.by != c.pid {
		return &opError{code: "locked", msg: "the room owner has made this room read-only"}
	}
	r.run.cancel()
	return nil
}

// resync sends the full current state to c, discarding its local edits.
func (r *Room) resync(c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.initLocked(c, -1)
	m.Resync = true
	c.enqueueJSON(m)
}

// isMonacoLang accepts language ids Monaco can highlight (superset of what
// the sandbox can run).
func isMonacoLang(id string) bool {
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '+' || c == '#') {
			return false
		}
	}
	return true
}
