package collab

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	"unicode/utf16"
)

type roomFile struct {
	ID        string           `json:"id"`
	Lang      string           `json:"lang"`
	Doc       string           `json:"doc"`
	Rev       int              `json:"rev"`
	OwnerHash string           `json:"owner_hash"`
	Locked    bool             `json:"locked"`
	Chat      []chatMsg        `json:"chat"`
	LastSeq   map[string]int64 `json:"last_seq"`
	Created   time.Time        `json:"created"`
	Updated   time.Time        `json:"updated"`
}

func (h *Hub) path(id string) string { return filepath.Join(h.dir, id+".json") }

// persist writes a dirty room atomically (temp file + rename).
func (h *Hub) persist(r *Room) {
	r.mu.Lock()
	if !r.dirty {
		r.mu.Unlock()
		return
	}
	f := roomFile{
		ID: r.id, Lang: r.lang, Doc: r.docString(), Rev: r.rev, OwnerHash: r.ownerHash,
		Locked: r.locked, Chat: r.chat, LastSeq: make(map[string]int64, len(r.lastSeq)),
		Created: r.created, Updated: r.updated,
	}
	for k, v := range r.lastSeq {
		f.LastSeq[k] = v
	}
	r.dirty = false
	r.mu.Unlock()

	b, err := json.Marshal(f)
	if err == nil {
		tmp := h.path(r.id) + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, h.path(r.id))
		}
	}
	if err != nil {
		h.log.Error("persist room", "room", r.id, "err", err)
		r.mu.Lock()
		r.dirty = true
		r.mu.Unlock()
	}
}

func (h *Hub) load(id string) (*Room, error) {
	b, err := os.ReadFile(h.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var f roomFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if time.Since(f.Updated) > h.cfg.RoomTTL {
		return nil, errNotFound
	}
	if f.LastSeq == nil {
		f.LastSeq = map[string]int64{}
	}
	return &Room{
		id: f.ID, hub: h, doc: utf16.Encode([]rune(f.Doc)), rev: f.Rev, logBase: f.Rev,
		lang: f.Lang, ownerHash: f.OwnerHash, locked: f.Locked, chat: f.Chat,
		lastSeq: f.LastSeq, created: f.Created, updated: f.Updated,
		peers: map[*conn]struct{}{}, idleSince: time.Now(),
	}, nil
}
