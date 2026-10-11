// Package collab implements real-time collaborative code rooms: a
// server-authoritative operational-transformation (OT) engine, presence,
// chat and shared code execution over WebSocket.
package collab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf16"
)

// Operation is a text edit in the ot.js format. Positions and lengths count
// UTF-16 code units, matching JavaScript strings and Monaco offsets. On the
// wire it is a JSON array whose elements are:
//
//	n > 0    retain n units
//	n < 0    delete -n units
//	"text"   insert text
type Operation struct {
	comps     []comp
	baseLen   int
	targetLen int
}

type compKind uint8

const (
	retainK compKind = iota + 1
	insertK
	deleteK
)

type comp struct {
	kind compKind
	n    int      // retain / delete length
	s    []uint16 // insert text
}

func (c comp) length() int {
	if c.kind == insertK {
		return len(c.s)
	}
	return c.n
}

var errBaseLength = errors.New("operation base length does not match document")

const maxComponents = 20000

func (o *Operation) Retain(n int) *Operation {
	if n <= 0 {
		return o
	}
	o.baseLen += n
	o.targetLen += n
	if l := len(o.comps); l > 0 && o.comps[l-1].kind == retainK {
		o.comps[l-1].n += n
	} else {
		o.comps = append(o.comps, comp{kind: retainK, n: n})
	}
	return o
}

func (o *Operation) Insert(s []uint16) *Operation {
	if len(s) == 0 {
		return o
	}
	o.targetLen += len(s)
	l := len(o.comps)
	switch {
	case l > 0 && o.comps[l-1].kind == insertK:
		o.comps[l-1].s = append(o.comps[l-1].s, s...)
	case l > 0 && o.comps[l-1].kind == deleteK:
		// Canonical form puts inserts before deletes.
		if l > 1 && o.comps[l-2].kind == insertK {
			o.comps[l-2].s = append(o.comps[l-2].s, s...)
		} else {
			o.comps = append(o.comps, o.comps[l-1])
			o.comps[l-1] = comp{kind: insertK, s: append([]uint16(nil), s...)}
		}
	default:
		o.comps = append(o.comps, comp{kind: insertK, s: append([]uint16(nil), s...)})
	}
	return o
}

func (o *Operation) Delete(n int) *Operation {
	if n <= 0 {
		return o
	}
	o.baseLen += n
	if l := len(o.comps); l > 0 && o.comps[l-1].kind == deleteK {
		o.comps[l-1].n += n
	} else {
		o.comps = append(o.comps, comp{kind: deleteK, n: n})
	}
	return o
}

func (o *Operation) BaseLen() int   { return o.baseLen }
func (o *Operation) TargetLen() int { return o.targetLen }

// IsNoop reports whether the operation leaves every document unchanged.
func (o *Operation) IsNoop() bool {
	return len(o.comps) == 0 || (len(o.comps) == 1 && o.comps[0].kind == retainK)
}

func (o *Operation) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("operation must be an array: %w", err)
	}
	if len(raw) > maxComponents {
		return errors.New("operation has too many components")
	}
	*o = Operation{}
	for _, r := range raw {
		r = bytes.TrimSpace(r)
		if len(r) > 0 && r[0] == '"' {
			var s string
			if err := json.Unmarshal(r, &s); err != nil {
				return err
			}
			if s == "" {
				return errors.New("empty insert")
			}
			o.Insert(utf16.Encode([]rune(s)))
			continue
		}
		var n int
		if err := json.Unmarshal(r, &n); err != nil {
			return errors.New("operation components must be integers or strings")
		}
		switch {
		case n > 0:
			o.Retain(n)
		case n < 0:
			o.Delete(-n)
		default:
			return errors.New("zero-length component")
		}
	}
	return nil
}

func (o *Operation) MarshalJSON() ([]byte, error) {
	out := make([]any, len(o.comps))
	for i, c := range o.comps {
		switch c.kind {
		case retainK:
			out[i] = c.n
		case deleteK:
			out[i] = -c.n
		case insertK:
			out[i] = string(utf16.Decode(c.s))
		}
	}
	return json.Marshal(out)
}

// Apply returns the document produced by applying o to doc.
func (o *Operation) Apply(doc []uint16) ([]uint16, error) {
	if len(doc) != o.baseLen {
		return nil, errBaseLength
	}
	out := make([]uint16, 0, o.targetLen)
	i := 0
	for _, c := range o.comps {
		switch c.kind {
		case retainK:
			out = append(out, doc[i:i+c.n]...)
			i += c.n
		case insertK:
			out = append(out, c.s...)
		case deleteK:
			i += c.n
		}
	}
	return out, nil
}

// Transform takes concurrent operations a and b (same base document) and
// returns a', b' such that apply(apply(d, a), b') == apply(apply(d, b), a').
// When both insert at the same position, a's insert is placed first.
func Transform(a, b *Operation) (*Operation, *Operation, error) {
	if a.baseLen != b.baseLen {
		return nil, nil, errBaseLength
	}
	ap, bp := &Operation{}, &Operation{}
	ia, ib := 0, 0
	var ca, cb *comp
	next := func(comps []comp, i *int) *comp {
		if *i >= len(comps) {
			return nil
		}
		c := comps[*i] // copy: we mutate lengths as we consume
		*i++
		return &c
	}
	ca, cb = next(a.comps, &ia), next(b.comps, &ib)
	for ca != nil || cb != nil {
		if ca != nil && ca.kind == insertK {
			ap.Insert(ca.s)
			bp.Retain(len(ca.s))
			ca = next(a.comps, &ia)
			continue
		}
		if cb != nil && cb.kind == insertK {
			ap.Retain(len(cb.s))
			bp.Insert(cb.s)
			cb = next(b.comps, &ib)
			continue
		}
		if ca == nil || cb == nil {
			return nil, nil, errors.New("transform: operations have mismatched lengths")
		}
		m := min(ca.n, cb.n)
		switch {
		case ca.kind == retainK && cb.kind == retainK:
			ap.Retain(m)
			bp.Retain(m)
		case ca.kind == deleteK && cb.kind == retainK:
			ap.Delete(m)
		case ca.kind == retainK && cb.kind == deleteK:
			bp.Delete(m)
		case ca.kind == deleteK && cb.kind == deleteK:
			// both deleted the same range
		}
		ca.n -= m
		cb.n -= m
		if ca.n == 0 {
			ca = next(a.comps, &ia)
		}
		if cb.n == 0 {
			cb = next(b.comps, &ib)
		}
	}
	return ap, bp, nil
}

// TransformIndex maps a cursor position through o. Inserts exactly at the
// cursor push it right (same rule as ot.js Selection.transform).
func (o *Operation) TransformIndex(idx int) int {
	pos, newIdx := 0, idx
	for _, c := range o.comps {
		if pos > idx {
			break
		}
		switch c.kind {
		case retainK:
			pos += c.n
		case insertK:
			newIdx += len(c.s)
		case deleteK:
			newIdx -= min(c.n, idx-pos)
			pos += c.n
		}
	}
	return max(newIdx, 0)
}
