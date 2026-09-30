package main

import "bytes"

// binaryCheckBytes mirrors git and libgit2: a blob is binary when a NUL byte
// appears in its first 8000 bytes. Binary files count as zero changed lines.
const binaryCheckBytes = 8000

// maxDiffWork bounds the Myers search. Beyond it lineDiffStat settles for an
// upper bound on the edit distance rather than stalling on pathological input.
const maxDiffWork = 200_000_000

func isBinary(content []byte) bool {
	return bytes.IndexByte(content[:min(len(content), binaryCheckBytes)], 0) >= 0
}

// splitLines splits content into lines, keeping each terminator so that a
// missing final newline counts as a changed line, as it does in git.
func splitLines(content []byte) [][]byte {
	var lines [][]byte
	for len(content) > 0 {
		i := bytes.IndexByte(content, '\n') + 1
		if i == 0 {
			i = len(content)
		}
		lines = append(lines, content[:i])
		content = content[i:]
	}
	return lines
}

// lineDiffStat counts the lines added and deleted by a minimal line diff from
// a to b. Every minimal diff has the same counts, so no alignment is needed:
// with edit distance D, adds+dels = D and adds-dels = len(b)-len(a).
func lineDiffStat(a, b []byte) (adds, dels int) {
	ids := map[string]int{}
	intern := func(lines [][]byte) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			id, ok := ids[string(l)]
			if !ok {
				id = len(ids)
				ids[string(l)] = id
			}
			out[i] = id
		}
		return out
	}
	x, y := intern(splitLines(a)), intern(splitLines(b))

	// Trim the common prefix and suffix.
	for len(x) > 0 && len(y) > 0 && x[0] == y[0] {
		x, y = x[1:], y[1:]
	}
	for len(x) > 0 && len(y) > 0 && x[len(x)-1] == y[len(y)-1] {
		x, y = x[:len(x)-1], y[:len(y)-1]
	}

	// Lines that never occur on the other side can't be part of any common
	// subsequence, so they are certain changes. Dropping them first keeps
	// rewrites such as regenerated lock files cheap to diff.
	inX, inY := map[int]bool{}, map[int]bool{}
	for _, id := range x {
		inX[id] = true
	}
	for _, id := range y {
		inY[id] = true
	}
	keep := func(s []int, other map[int]bool) ([]int, int) {
		kept := s[:0:0]
		for _, id := range s {
			if other[id] {
				kept = append(kept, id)
			}
		}
		return kept, len(s) - len(kept)
	}
	x, dels = keep(x, inY)
	y, adds = keep(y, inX)

	d := editDistance(x, y)
	sizeDiff := len(y) - len(x)
	return adds + (d+sizeDiff)/2, dels + (d-sizeDiff)/2
}

// editDistance returns the insert/delete edit distance between x and y using
// Myers' O(ND) algorithm, or an upper bound on it once maxDiffWork is spent.
func editDistance(x, y []int) int {
	n, m := len(x), len(y)
	if n == 0 || m == 0 {
		return n + m
	}
	limit := n + m
	offset := limit + 1
	v := make([]int, 2*limit+3)
	work := 0
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var i int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				i = v[offset+k+1]
			} else {
				i = v[offset+k-1] + 1
			}
			j := i - k
			for i < n && j < m && x[i] == y[j] {
				i, j = i+1, j+1
			}
			v[offset+k] = i
			if i >= n && j >= m {
				return d
			}
		}
		work += d + 1
		if work > maxDiffWork {
			// Any path reaching (i, j) in d edits finishes in at most
			// (n-i)+(m-j) more, so take the best such bound.
			best := n + m
			for k := -d; k <= d; k += 2 {
				i := min(v[offset+k], n)
				j := min(max(i-k, 0), m)
				best = min(best, d+(n-i)+(m-j))
			}
			return best
		}
	}
	return n + m
}
