package collab

import (
	"encoding/json"
	"math/rand"
	"slices"
	"testing"
	"unicode/utf16"
)

func u16(s string) []uint16 { return utf16.Encode([]rune(s)) }

var alphabet = []rune("abcXYZ \n😀é")

func randomOp(r *rand.Rand, doc []uint16) *Operation {
	op := &Operation{}
	for i := 0; i < len(doc); {
		// Stay on surrogate-pair boundaries like a real editor would.
		n := 1 + r.Intn(max(1, (len(doc)-i)/2+1))
		end := min(i+n, len(doc))
		if end < len(doc) && utf16.IsSurrogate(rune(doc[end])) && end > 0 && doc[end] >= 0xDC00 {
			end++
		}
		switch r.Intn(3) {
		case 0:
			op.Retain(end - i)
		case 1:
			op.Delete(end - i)
		case 2:
			op.Insert(randomText(r))
			op.Retain(end - i)
		}
		i = end
	}
	if r.Intn(2) == 0 {
		op.Insert(randomText(r))
	}
	return op
}

func randomText(r *rand.Rand) []uint16 {
	n := 1 + r.Intn(4)
	var rs []rune
	for range n {
		rs = append(rs, alphabet[r.Intn(len(alphabet))])
	}
	return u16(string(rs))
}

func TestTransformConvergence(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	for iter := range 20000 {
		doc := randomText(r)
		for range r.Intn(6) {
			doc = append(doc, randomText(r)...)
		}
		a, b := randomOp(r, doc), randomOp(r, doc)
		ap, bp, err := Transform(a, b)
		if err != nil {
			t.Fatalf("iter %d: transform: %v", iter, err)
		}
		da, err := a.Apply(doc)
		if err != nil {
			t.Fatal(err)
		}
		dab, err := bp.Apply(da)
		if err != nil {
			t.Fatalf("iter %d: apply b': %v", iter, err)
		}
		db, _ := b.Apply(doc)
		dba, err := ap.Apply(db)
		if err != nil {
			t.Fatalf("iter %d: apply a': %v", iter, err)
		}
		if !slices.Equal(dab, dba) {
			t.Fatalf("iter %d: diverged:\n%q\n%q", iter, string(utf16.Decode(dab)), string(utf16.Decode(dba)))
		}
	}
}

func TestJSONRoundTrip(t *testing.T) {
	var op Operation
	if err := json.Unmarshal([]byte(`[3,"hi 😀",-2,1]`), &op); err != nil {
		t.Fatal(err)
	}
	if op.BaseLen() != 6 || op.TargetLen() != 4+5 {
		t.Fatalf("lengths base=%d target=%d", op.BaseLen(), op.TargetLen())
	}
	got, err := op.Apply(u16("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if string(utf16.Decode(got)) != "abchi 😀f" {
		t.Fatalf("got %q", string(utf16.Decode(got)))
	}
	b, _ := json.Marshal(&op)
	if string(b) != `[3,"hi 😀",-2,1]` {
		t.Fatalf("marshal %s", b)
	}
}

func TestCanonicalInsertBeforeDelete(t *testing.T) {
	op := (&Operation{}).Retain(1).Delete(2).Insert(u16("x"))
	b, _ := json.Marshal(op)
	if string(b) != `[1,"x",-2]` {
		t.Fatalf("got %s", b)
	}
}

func TestRejectsBadOps(t *testing.T) {
	for _, in := range []string{`{}`, `[0]`, `[""]`, `[1.5]`, `[true]`, `"x"`} {
		var op Operation
		if err := json.Unmarshal([]byte(in), &op); err == nil {
			t.Errorf("%s: expected error", in)
		}
	}
	var op Operation
	_ = json.Unmarshal([]byte(`[5]`), &op)
	if _, err := op.Apply(u16("abc")); err == nil {
		t.Error("expected base length error")
	}
}

func TestTransformIndex(t *testing.T) {
	op := (&Operation{}).Retain(2).Insert(u16("XX")).Delete(1).Retain(3)
	cases := map[int]int{0: 0, 1: 1, 2: 4, 3: 4, 4: 5, 6: 7}
	for in, want := range cases {
		if got := op.TransformIndex(in); got != want {
			t.Errorf("TransformIndex(%d) = %d, want %d", in, got, want)
		}
	}
}
