package projsync

import "strings"

// Three-way text merge: if this PC and another PC changed different lines
// of the same file since the base, both changes are kept. Overlapping
// changes are a real clash and are left to the project's clash rule.

const maxMergeEdits = 4000 // beyond this, the files are too different to merge line by line

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// hunk replaces base lines [start,end) with lines.
type hunk struct {
	start, end int
	lines      []string
}

// diff returns the hunks that turn a into b (Myers' algorithm), or false if
// the files differ too much.
func diff(a, b []string) ([]hunk, bool) {
	n, m := len(a), len(b)
	maxD := min(n+m, maxMergeEdits)
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	type snap struct {
		lo   int
		vals []int
	}
	var trace []snap
	found := false
	for d := 0; d <= maxD && !found; d++ {
		lo := -(d + 1)
		trace = append(trace, snap{lo, append([]int(nil), v[off+lo:off+d+2]...)})
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	if !found {
		return nil, false
	}
	// Backtrack into a list of edits, last first.
	type edit struct {
		op   byte // '=', '-', '+'
		x, y int
	}
	var edits []edit
	x, y := n, m
	for d := len(trace) - 1; d >= 0; d-- {
		s := trace[d]
		get := func(k int) int { return s.vals[k-s.lo] }
		k := x - y
		var prevK int
		if k == -d || (k != d && get(k-1) < get(k+1)) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := get(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			edits = append(edits, edit{'=', x - 1, y - 1})
			x, y = x-1, y-1
		}
		if d > 0 {
			if x == prevX {
				edits = append(edits, edit{'+', x, y - 1})
				y--
			} else {
				edits = append(edits, edit{'-', x - 1, y})
				x--
			}
		}
	}
	// Forward order, grouped into hunks.
	var hunks []hunk
	var cur *hunk
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		switch e.op {
		case '=':
			if cur != nil {
				hunks = append(hunks, *cur)
				cur = nil
			}
		case '-':
			if cur == nil {
				cur = &hunk{start: e.x, end: e.x}
			}
			cur.end = e.x + 1
		case '+':
			if cur == nil {
				cur = &hunk{start: e.x, end: e.x}
			}
			cur.lines = append(cur.lines, b[e.y])
		}
	}
	if cur != nil {
		hunks = append(hunks, *cur)
	}
	return hunks, true
}

func sameHunk(a, b hunk) bool {
	if a.start != b.start || a.end != b.end || len(a.lines) != len(b.lines) {
		return false
	}
	for i := range a.lines {
		if a.lines[i] != b.lines[i] {
			return false
		}
	}
	return true
}

// merge3 combines two edited versions of base. ok is false when the edits
// overlap (a real clash) or can't be worked out.
func merge3(base, mine, theirs []byte) (merged []byte, ok bool) {
	b := splitLines(string(base))
	hm, ok1 := diff(b, splitLines(string(mine)))
	ht, ok2 := diff(b, splitLines(string(theirs)))
	if !ok1 || !ok2 {
		return nil, false
	}
	var out strings.Builder
	pos, i, j := 0, 0, 0
	for i < len(hm) || j < len(ht) {
		var h hunk
		switch {
		case j >= len(ht):
			h, i = hm[i], i+1
		case i >= len(hm):
			h, j = ht[j], j+1
		default:
			x, y := hm[i], ht[j]
			if x.start <= y.end && y.start <= x.end { // touching or overlapping
				if !sameHunk(x, y) {
					return nil, false
				}
				h, i, j = x, i+1, j+1
			} else if x.start < y.start {
				h, i = x, i+1
			} else {
				h, j = y, j+1
			}
		}
		if h.start < pos {
			return nil, false
		}
		for _, l := range b[pos:h.start] {
			out.WriteString(l)
		}
		for _, l := range h.lines {
			out.WriteString(l)
		}
		pos = h.end
	}
	for _, l := range b[pos:] {
		out.WriteString(l)
	}
	return []byte(out.String()), true
}
