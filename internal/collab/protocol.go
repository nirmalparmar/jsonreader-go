package collab

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
)

// inMsg is any client -> server message; "t" selects the type.
//
//	join  {sid, name, rev, owner_token}   must be the first message
//	op    {rev, seq, op, sel}             edit based on server revision rev
//	sel   {sel}                           cursor / selection update
//	lang  {lang}                          change room language
//	name  {name}                          rename yourself
//	chat  {text}
//	run   {stdin}                         run the room's code for everyone
//	stop  {}                              stop the current run
//	lock  {locked}                        owner only: make room read-only
//	ping  {}
type inMsg struct {
	T          string          `json:"t"`
	SID        string          `json:"sid"`
	Name       string          `json:"name"`
	Rev        *int            `json:"rev"`
	Seq        int64           `json:"seq"`
	Op         json.RawMessage `json:"op"`
	Sel        json.RawMessage `json:"sel"`
	Lang       string          `json:"lang"`
	Text       string          `json:"text"`
	Stdin      string          `json:"stdin"`
	Locked     bool            `json:"locked"`
	OwnerToken string          `json:"owner_token"`
}

type peerInfo struct {
	PID   string     `json:"pid"`
	Name  string     `json:"name"`
	Color string     `json:"color"`
	Owner bool       `json:"owner"`
	Sel   *selection `json:"sel,omitempty"`
}

type chatMsg struct {
	PID   string    `json:"pid"`
	Name  string    `json:"name"`
	Color string    `json:"color"`
	Text  string    `json:"text"`
	TS    time.Time `json:"ts"`
}

type catchupEntry struct {
	Rev int        `json:"rev"`
	Op  *Operation `json:"op"`
	Own bool       `json:"own,omitempty"`
	PID string     `json:"pid,omitempty"`
}

type langInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type initMsg struct {
	T         string         `json:"t"` // "init"
	Room      string         `json:"room"`
	You       peerInfo       `json:"you"`
	Rev       int            `json:"rev"`
	Doc       *string        `json:"doc,omitempty"`     // full document (fresh join / resync)
	Catchup   []catchupEntry `json:"catchup,omitempty"` // ops since the client's rev
	Resync    bool           `json:"resync,omitempty"`
	LastSeq   int64          `json:"last_seq"`
	Lang      string         `json:"lang"`
	Locked    bool           `json:"locked"`
	Peers     []peerInfo     `json:"peers"`
	Chat      []chatMsg      `json:"chat"`
	Languages []langInfo     `json:"languages"`
	Running   bool           `json:"running"`
	Output    []outChunk     `json:"output,omitempty"`
	Limits    map[string]int `json:"limits"`
}

type outChunk struct {
	Stream string `json:"stream"`
	Phase  string `json:"phase,omitempty"`
	Data   string `json:"data"`
}

// selection is a list of [anchor, head] offsets in UTF-16 units.
type selection struct {
	Ranges [][2]int `json:"ranges"`
}

const maxRanges = 32

func parseSelection(raw json.RawMessage, docLen int) (*selection, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s selection
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if len(s.Ranges) > maxRanges {
		s.Ranges = s.Ranges[:maxRanges]
	}
	for i, r := range s.Ranges {
		for j := range 2 {
			if r[j] < 0 || r[j] > docLen {
				return nil, errors.New("selection out of range")
			}
		}
		s.Ranges[i] = r
	}
	return &s, nil
}

func (s *selection) transform(op *Operation) {
	if s == nil {
		return
	}
	for i := range s.Ranges {
		s.Ranges[i][0] = op.TransformIndex(s.Ranges[i][0])
		s.Ranges[i][1] = op.TransformIndex(s.Ranges[i][1])
	}
}

func (s *selection) clone() *selection {
	if s == nil {
		return nil
	}
	c := &selection{Ranges: make([][2]int, len(s.Ranges))}
	copy(c.Ranges, s.Ranges)
	return c
}

// cleanText strips control characters (except newline/tab when multiline)
// and trims to max runes.
func cleanText(s string, maxRunes int, multiline bool) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= maxRunes {
			break
		}
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			continue
		}
		if r == '‮' || r == '‭' || r == '⁦' || r == '⁧' || r == '⁨' || r == '⁩' {
			continue // bidi overrides could spoof names
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

var palette = []string{
	"#e11d48", "#2563eb", "#16a34a", "#d97706", "#9333ea", "#0891b2",
	"#db2777", "#4f46e5", "#65a30d", "#ea580c", "#0d9488", "#7c3aed",
}
