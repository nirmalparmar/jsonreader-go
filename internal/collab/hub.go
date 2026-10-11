package collab

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/coder/websocket"

	"github.com/nirmalparmar/common-service/internal/compiler"
	"github.com/nirmalparmar/common-service/internal/config"
	"github.com/nirmalparmar/common-service/internal/httpx"
)

var (
	roomIDRe    = regexp.MustCompile(`^[A-Za-z0-9]{8,32}$`)
	errNotFound = errors.New("room not found")
)

// Hub owns all rooms: lookup, creation, persistence and expiry.
type Hub struct {
	cfg        config.CollabConfig
	dir        string
	comp       *compiler.Service
	originHost []string
	log        *slog.Logger
	runLimiter *httpx.RateLimiter

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	rooms    map[string]*Room
	loading  map[string]*sync.Mutex
	onDisk   atomic.Int64
	langOnce sync.Once
	langs    []langInfo
}

func NewHub(cfg config.CollabConfig, dataDir string, comp *compiler.Service, originHosts []string, log *slog.Logger) (*Hub, error) {
	dir := filepath.Join(dataDir, "rooms")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("collab data dir: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		cfg: cfg, dir: dir, comp: comp, originHost: originHosts, log: log,
		runLimiter: httpx.NewRateLimiter(cfg.RunsPerMinute, max(cfg.RunsPerMinute/2, 3)),
		ctx:        ctx, cancel: cancel,
		rooms:   map[string]*Room{},
		loading: map[string]*sync.Mutex{},
	}
	if entries, err := os.ReadDir(dir); err == nil {
		h.onDisk.Store(int64(len(entries)))
	}
	h.wg.Add(2)
	go h.persistLoop()
	go h.janitor()
	return h, nil
}

// Close flushes every room to disk and disconnects all peers.
func (h *Hub) Close() {
	h.cancel()
	h.wg.Wait()
	h.mu.Lock()
	rooms := make([]*Room, 0, len(h.rooms))
	for _, r := range h.rooms {
		rooms = append(rooms, r)
	}
	h.mu.Unlock()
	for _, r := range rooms {
		r.mu.Lock()
		for c := range r.peers {
			go c.close(websocket.StatusGoingAway, "server restarting")
		}
		r.mu.Unlock()
		h.persist(r)
	}
}

func (h *Hub) languages() []langInfo {
	h.langOnce.Do(func() {
		h.langs = []langInfo{}
		if h.comp != nil {
			for _, l := range h.comp.Languages() {
				h.langs = append(h.langs, langInfo{ID: l.ID, Name: l.Name})
			}
		}
	})
	return h.langs
}

func newRoomID() string {
	const alphabet = "23456789abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ" // no 0/O/1/l/I
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (h *Hub) create(lang, content string) (*Room, string, error) {
	if h.onDisk.Load()+int64(h.memCount()) >= int64(h.cfg.MaxRooms) {
		return nil, "", errors.New("room capacity reached")
	}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	token := newToken()
	now := time.Now().UTC()
	r := &Room{
		id: newRoomID(), hub: h, doc: utf16.Encode([]rune(content)), lang: lang,
		ownerHash: hashToken(token), created: now, updated: now, dirty: true,
		lastSeq: map[string]int64{}, peers: map[*conn]struct{}{}, idleSince: time.Now(),
	}
	h.mu.Lock()
	h.rooms[r.id] = r
	h.mu.Unlock()
	h.persist(r)
	h.onDisk.Add(1)
	return r, token, nil
}

func (h *Hub) memCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms)
}

// get returns a live room, loading it from disk on first access.
func (h *Hub) get(id string) (*Room, error) {
	if !roomIDRe.MatchString(id) {
		return nil, errNotFound
	}
	h.mu.Lock()
	if r, ok := h.rooms[id]; ok {
		h.mu.Unlock()
		return r, nil
	}
	l, ok := h.loading[id]
	if !ok {
		l = &sync.Mutex{}
		h.loading[id] = l
	}
	h.mu.Unlock()

	l.Lock()
	defer l.Unlock()
	h.mu.Lock()
	if r, ok := h.rooms[id]; ok {
		h.mu.Unlock()
		return r, nil
	}
	h.mu.Unlock()

	r, err := h.load(id)
	h.mu.Lock()
	delete(h.loading, id)
	if err == nil {
		h.rooms[id] = r
	}
	h.mu.Unlock()
	return r, err
}

func (h *Hub) persistLoop() {
	defer h.wg.Done()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
			h.mu.Lock()
			rooms := make([]*Room, 0, len(h.rooms))
			for _, r := range h.rooms {
				rooms = append(rooms, r)
			}
			h.mu.Unlock()
			for _, r := range rooms {
				h.persist(r)
			}
		}
	}
}

// janitor unloads idle rooms from memory and deletes expired room files.
func (h *Hub) janitor() {
	defer h.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastSweep := time.Time{}
	for {
		select {
		case <-h.ctx.Done():
			return
		case now := <-t.C:
			h.mu.Lock()
			var idle []*Room
			for _, r := range h.rooms {
				r.mu.Lock()
				if len(r.peers) == 0 && r.run == nil && !r.idleSince.IsZero() && now.Sub(r.idleSince) > h.cfg.IdleUnload {
					idle = append(idle, r)
				}
				r.mu.Unlock()
			}
			h.mu.Unlock()
			for _, r := range idle {
				h.unload(r)
			}
			if now.Sub(lastSweep) > time.Hour {
				lastSweep = now
				h.sweepExpired(now)
			}
		}
	}
}

func (h *Hub) unload(r *Room) {
	h.persist(r)
	h.mu.Lock()
	defer h.mu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.peers) > 0 || r.run != nil {
		return // someone joined meanwhile
	}
	r.closed = true
	delete(h.rooms, r.id)
}

func (h *Hub) sweepExpired(now time.Time) {
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		return
	}
	var n int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		h.mu.Lock()
		_, live := h.rooms[id]
		h.mu.Unlock()
		if !live && now.Sub(info.ModTime()) > h.cfg.RoomTTL {
			_ = os.Remove(filepath.Join(h.dir, e.Name()))
			continue
		}
		n++
	}
	h.onDisk.Store(n)
}

// ---------------------------------------------------------------------------
// HTTP

type createReq struct {
	Language string `json:"language"`
	Content  string `json:"content"`
}

// Routes registers:
//
//	POST /v1/rooms             create a room -> {id, owner_token}
//	GET  /v1/rooms/{id}        room metadata
//	GET  /v1/rooms/{id}/raw    current document as text/plain
//	GET  /v1/rooms/{id}/ws     WebSocket collaboration channel
func (h *Hub) Routes(mux *http.ServeMux, createLimit, connectLimit func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/rooms", createLimit(http.HandlerFunc(h.handleCreate)))
	mux.HandleFunc("GET /v1/rooms/{id}", h.handleInfo)
	mux.HandleFunc("GET /v1/rooms/{id}/raw", h.handleRaw)
	mux.Handle("GET /v1/rooms/{id}/ws", connectLimit(http.HandlerFunc(h.handleWS)))
}

func (h *Hub) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if !httpx.DecodeJSON(w, r, int64(h.cfg.MaxDocBytes)+4096, &req) {
		return
	}
	lang := strings.ToLower(strings.TrimSpace(req.Language))
	if lang == "" {
		lang = defaultLang
	}
	if len(lang) > 32 || !isMonacoLang(lang) {
		httpx.Error(w, http.StatusBadRequest, "bad_lang", "invalid language")
		return
	}
	if len(utf16.Encode([]rune(req.Content))) > h.cfg.MaxDocBytes/2 {
		httpx.Error(w, http.StatusRequestEntityTooLarge, "too_large", "content too large")
		return
	}
	room, token, err := h.create(lang, req.Content)
	if err != nil {
		httpx.Error(w, http.StatusServiceUnavailable, "capacity", err.Error())
		return
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"id": room.id, "owner_token": token, "language": lang})
}

func (h *Hub) handleInfo(w http.ResponseWriter, r *http.Request) {
	room, err := h.get(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	httpx.JSON(w, http.StatusOK, map[string]any{
		"id": room.id, "language": room.lang, "locked": room.locked, "peers": len(room.peers),
		"rev": room.rev, "size": len(room.doc), "created": room.created, "updated": room.updated,
	})
}

func (h *Hub) handleRaw(w http.ResponseWriter, r *http.Request) {
	room, err := h.get(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	room.mu.Lock()
	doc := room.docString()
	room.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(doc))
}

func (h *Hub) handleWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !roomIDRe.MatchString(id) {
		httpx.Error(w, http.StatusNotFound, "not_found", "room not found")
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  h.originHost, // rejects cross-site WebSocket hijacking
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return // Accept already wrote the response
	}
	ws.SetReadLimit(int64(h.cfg.MaxDocBytes)*3 + 64<<10)
	c := &conn{
		ws: ws, ip: httpx.ClientIP(r), pid: newPID(),
		send: make(chan []byte, sendQueue), done: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	stop := context.AfterFunc(r.Context(), cancel)
	defer stop()
	c.serve(ctx, h, id)
}
