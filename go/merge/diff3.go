// Package merge implements a pure-Go line-based LCS diff, diff3 merge,
// 2-way minimal fallback, and N-way conflict folding engine supporting both
// Markdown backtick-fenced markers and plain text conflict markers.
package merge

import "slices"

// maxExactDPCells caps the flat 1D DP matrix used by exactLCS (262,144 cells = 1 MiB of int32).
// Spans larger than this use Patience unique-line anchoring and linear-space Hirschberg splitting.
const maxExactDPCells = 262_144

// maxHirschbergCells caps total work on un-anchorable repetitive spans (16M cells)
// so pathological multi-thousand-line files without unique lines never freeze Wasm.
const maxHirschbergCells = 16_000_000

// DiffBlock represents a differing hunk between two line slices (buf1 and buf2)
// as (offset, length) pairs in half-open index space.
type DiffBlock struct {
	Buf1Start int
	Buf1Len   int
	Buf2Start int
	Buf2Len   int
}

// ConflictHunk represents a 3-way conflict span across base (O), local (A), and remote (B).
type ConflictHunk struct {
	O []string
	A []string
	B []string
}

// MergeRegion is either a clean line slice (OK) or a 3-way conflict hunk.
type MergeRegion struct {
	OK       []string
	Conflict *ConflictHunk
}

// DiffIndices computes minimal contiguous differing hunks between buf1 and buf2 using LCS.
func DiffIndices(buf1, buf2 []string) []DiffBlock {
	n := len(buf1)
	m := len(buf2)

	prefix := 0
	for prefix < n && prefix < m && buf1[prefix] == buf2[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < (n-prefix) && suffix < (m-prefix) && buf1[n-1-suffix] == buf2[m-1-suffix] {
		suffix++
	}

	sub1 := buf1[prefix : n-suffix]
	sub2 := buf2[prefix : m-suffix]

	matches := lcsPairs(sub1, sub2)
	matches = append(matches, matchPair{i: len(sub1), j: len(sub2)})

	var blocks []DiffBlock
	cur1, cur2 := 0, 0
	for _, p := range matches {
		if cur1 < p.i || cur2 < p.j {
			blocks = append(blocks, DiffBlock{
				Buf1Start: prefix + cur1,
				Buf1Len:   p.i - cur1,
				Buf2Start: prefix + cur2,
				Buf2Len:   p.j - cur2,
			})
		}
		cur1 = p.i + 1
		cur2 = p.j + 1
	}
	return blocks
}

type matchPair struct {
	i int
	j int
}

func lcsPairs(a, b []string) []matchPair {
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return nil
	}
	idA, idB := internLines(a, b)
	return lcsIDs(idA, idB, 0, 0)
}

func internLines(a, b []string) ([]int32, []int32) {
	sym := make(map[string]int32, len(a))
	idA := make([]int32, len(a))
	var nextID int32 = 1
	for i, s := range a {
		id, ok := sym[s]
		if !ok {
			id = nextID
			nextID++
			sym[s] = id
		}
		idA[i] = id
	}
	idB := make([]int32, len(b))
	for j, s := range b {
		if id, ok := sym[s]; ok {
			idB[j] = id
		}
	}
	return idA, idB
}

func lcsIDs(a, b []int32, offA, offB int) []matchPair {
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return nil
	}

	// Trim matching prefix and suffix of this subproblem.
	pre := 0
	for pre < la && pre < lb && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < (la-pre) && suf < (lb-pre) && a[la-1-suf] == b[lb-1-suf] {
		suf++
	}

	var out []matchPair
	if pre > 0 || suf > 0 {
		out = make([]matchPair, 0, pre+suf)
		for k := range pre {
			out = append(out, matchPair{i: offA + k, j: offB + k})
		}
	}

	midA := a[pre : la-suf]
	midB := b[pre : lb-suf]
	if len(midA) > 0 && len(midB) > 0 {
		midPairs := lcsCore(midA, midB, offA+pre, offB+pre)
		out = append(out, midPairs...)
	}

	for k := range suf {
		out = append(out, matchPair{
			i: offA + la - suf + k,
			j: offB + lb - suf + k,
		})
	}
	return out
}

func lcsCore(a, b []int32, offA, offB int) []matchPair {
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return nil
	}
	if la*lb <= maxExactDPCells {
		return exactLCS(a, b, offA, offB)
	}

	// Split large spans on Patience unique common lines first (O(N log N)).
	if anchors := patienceAnchors(a, b); len(anchors) > 0 {
		var out []matchPair
		curA, curB := 0, 0
		for _, anc := range anchors {
			if anc.i > curA && anc.j > curB {
				out = append(out, lcsIDs(a[curA:anc.i], b[curB:anc.j], offA+curA, offB+curB)...)
			}
			out = append(out, matchPair{i: offA + anc.i, j: offB + anc.j})
			curA = anc.i + 1
			curB = anc.j + 1
		}
		if curA < la && curB < lb {
			out = append(out, lcsIDs(a[curA:], b[curB:], offA+curA, offB+curB)...)
		}
		return out
	}

	if la*lb <= maxHirschbergCells {
		fwd := make([]int32, lb+1)
		bwd := make([]int32, lb+1)
		return hirschbergLCS(a, b, offA, offB, fwd, bwd)
	}

	// Pathological huge un-anchorable span: coalesce as a single replacement hunk
	// so Wasm never freezes or runs out of memory.
	return nil
}

func exactLCS(a, b []int32, offA, offB int) []matchPair {
	la, lb := len(a), len(b)
	stride := lb + 1
	dp := make([]int32, (la+1)*stride)

	for i := la - 1; i >= 0; i-- {
		row := i * stride
		nextRow := (i + 1) * stride
		ai := a[i]
		for j := lb - 1; j >= 0; j-- {
			if ai == b[j] {
				dp[row+j] = dp[nextRow+j+1] + 1
			} else if dp[nextRow+j] >= dp[row+j+1] {
				dp[row+j] = dp[nextRow+j]
			} else {
				dp[row+j] = dp[row+j+1]
			}
		}
	}

	pairs := make([]matchPair, 0, dp[0])
	i, j := 0, 0
	for i < la && j < lb {
		if a[i] == b[j] {
			pairs = append(pairs, matchPair{i: offA + i, j: offB + j})
			i++
			j++
		} else if dp[(i+1)*stride+j] >= dp[i*stride+j+1] {
			i++
		} else {
			j++
		}
	}
	return pairs
}

func hirschbergLCS(a, b []int32, offA, offB int, fwd, bwd []int32) []matchPair {
	la, lb := len(a), len(b)
	if la == 0 || lb == 0 {
		return nil
	}
	if la*lb <= maxExactDPCells {
		return exactLCS(a, b, offA, offB)
	}
	if la == 1 {
		target := a[0]
		for j := range lb {
			if b[j] == target {
				return []matchPair{{i: offA, j: offB + j}}
			}
		}
		return nil
	}

	mid := la / 2
	top := a[:mid]
	bot := a[mid:]

	// Forward LCS lengths for top[0:mid] against b[0:lb].
	clear(fwd[:lb+1])
	for i := range len(top) {
		ai := top[i]
		var prevDiag int32
		for j := range lb {
			temp := fwd[j+1]
			if ai == b[j] {
				fwd[j+1] = prevDiag + 1
			} else if fwd[j] > fwd[j+1] {
				fwd[j+1] = fwd[j]
			}
			prevDiag = temp
		}
	}

	// Backward LCS lengths for bot[0:la-mid] against b[0:lb].
	clear(bwd[:lb+1])
	for i := len(bot) - 1; i >= 0; i-- {
		ai := bot[i]
		var prevDiag int32
		for j := lb - 1; j >= 0; j-- {
			temp := bwd[j]
			if ai == b[j] {
				bwd[j] = prevDiag + 1
			} else if bwd[j+1] > bwd[j] {
				bwd[j] = bwd[j+1]
			}
			prevDiag = temp
		}
	}

	splitJ := 0
	best := int32(-1)
	for j := 0; j <= lb; j++ {
		if score := fwd[j] + bwd[j]; score > best {
			best = score
			splitJ = j
		}
	}

	left := hirschbergLCS(top, b[:splitJ], offA, offB, fwd, bwd)
	right := hirschbergLCS(bot, b[splitJ:], offA+mid, offB+splitJ, fwd, bwd)
	return append(left, right...)
}

// patienceAnchors returns the Longest Increasing Subsequence of lines occurring
// exactly once in a and once in b.
func patienceAnchors(a, b []int32) []matchPair {
	posA := make(map[int32]int, len(a))
	for i, id := range a {
		if _, exists := posA[id]; exists {
			posA[id] = -1
		} else {
			posA[id] = i
		}
	}
	posB := make(map[int32]int, len(b))
	for j, id := range b {
		if pa, ok := posA[id]; !ok || pa < 0 {
			continue
		}
		if _, exists := posB[id]; exists {
			posB[id] = -1
		} else {
			posB[id] = j
		}
	}

	var unique []matchPair
	for i, id := range a {
		if posA[id] == i {
			if j, ok := posB[id]; ok && j >= 0 {
				unique = append(unique, matchPair{i: i, j: j})
			}
		}
	}
	if len(unique) == 0 {
		return nil
	}

	// Longest Increasing Subsequence over unique[k].j (since unique[k].i is strictly increasing).
	tails := make([]int, 0, len(unique))
	prev := make([]int, len(unique))
	for k := range prev {
		prev[k] = -1
	}

	for k, p := range unique {
		lo, hi := 0, len(tails)
		for lo < hi {
			mid := (lo + hi) / 2
			if unique[tails[mid]].j < p.j {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		if lo > 0 {
			prev[k] = tails[lo-1]
		}
		if lo == len(tails) {
			tails = append(tails, k)
		} else {
			tails[lo] = k
		}
	}

	out := make([]matchPair, len(tails))
	cur := tails[len(tails)-1]
	for idx := len(tails) - 1; idx >= 0; idx-- {
		out[idx] = unique[cur]
		cur = prev[cur]
	}
	return out
}

// Diff3Merge performs a 3-way line merge of a (local) and b (remote) against o (base).
func Diff3Merge(a, o, b []string) []MergeRegion {
	hunksA := DiffIndices(o, a)
	hunksB := DiffIndices(o, b)

	var regions []MergeRegion
	basePos := 0
	ia, ib := 0, 0
	deltaA, deltaB := 0, 0

	emitOK := func(lines []string) {
		if len(lines) == 0 {
			return
		}
		copied := slices.Clone(lines)
		if n := len(regions); n > 0 && regions[n-1].Conflict == nil {
			regions[n-1].OK = append(regions[n-1].OK, copied...)
			return
		}
		regions = append(regions, MergeRegion{OK: copied})
	}

	for ia < len(hunksA) || ib < len(hunksB) {
		var nextAStart, nextBStart int
		hasA := ia < len(hunksA)
		hasB := ib < len(hunksB)
		if hasA {
			nextAStart = hunksA[ia].Buf1Start
		}
		if hasB {
			nextBStart = hunksB[ib].Buf1Start
		}

		var spanStart, spanEnd int
		var sideAFirst bool
		if hasA && (!hasB || nextAStart <= nextBStart) {
			spanStart = hunksA[ia].Buf1Start
			spanEnd = spanStart + hunksA[ia].Buf1Len
			sideAFirst = true
		} else {
			spanStart = hunksB[ib].Buf1Start
			spanEnd = spanStart + hunksB[ib].Buf1Len
			sideAFirst = false
		}

		if spanStart > basePos {
			emitOK(o[basePos:spanStart])
		}

		aStart := spanStart + deltaA
		bStart := spanStart + deltaB
		usedA := false
		usedB := false

		if sideAFirst {
			usedA = true
			deltaA += hunksA[ia].Buf2Len - hunksA[ia].Buf1Len
			ia++
		} else {
			usedB = true
			deltaB += hunksB[ib].Buf2Len - hunksB[ib].Buf1Len
			ib++
		}

		for {
			expanded := false
			for ia < len(hunksA) && hunksOverlap(spanStart, spanEnd, hunksA[ia].Buf1Start, hunksA[ia].Buf1Start+hunksA[ia].Buf1Len) {
				usedA = true
				if end := hunksA[ia].Buf1Start + hunksA[ia].Buf1Len; end > spanEnd {
					spanEnd = end
					expanded = true
				}
				deltaA += hunksA[ia].Buf2Len - hunksA[ia].Buf1Len
				ia++
			}
			for ib < len(hunksB) && hunksOverlap(spanStart, spanEnd, hunksB[ib].Buf1Start, hunksB[ib].Buf1Start+hunksB[ib].Buf1Len) {
				usedB = true
				if end := hunksB[ib].Buf1Start + hunksB[ib].Buf1Len; end > spanEnd {
					spanEnd = end
					expanded = true
				}
				deltaB += hunksB[ib].Buf2Len - hunksB[ib].Buf1Len
				ib++
			}
			if !expanded {
				break
			}
		}

		aEnd := spanEnd + deltaA
		bEnd := spanEnd + deltaB
		oSlice := o[spanStart:spanEnd]
		aSlice := a[aStart:aEnd]
		bSlice := b[bStart:bEnd]

		switch {
		case !usedA && usedB:
			emitOK(bSlice)
		case usedA && !usedB:
			emitOK(aSlice)
		case slices.Equal(aSlice, bSlice):
			emitOK(aSlice)
		default:
			regions = append(regions, MergeRegion{
				Conflict: &ConflictHunk{
					O: slices.Clone(oSlice),
					A: slices.Clone(aSlice),
					B: slices.Clone(bSlice),
				},
			})
		}

		basePos = spanEnd
	}

	if basePos < len(o) {
		emitOK(o[basePos:])
	}
	return regions
}

func hunksOverlap(spanStart, spanEnd, hStart, hEnd int) bool {
	if spanStart == spanEnd && hStart == hEnd {
		return hStart == spanStart
	}
	if spanStart == spanEnd {
		return hStart <= spanStart && spanStart < hEnd
	}
	if hStart == hEnd {
		return spanStart <= hStart && hStart < spanEnd
	}
	return hStart < spanEnd && spanStart < hEnd
}
