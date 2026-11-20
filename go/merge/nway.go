package merge

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// MarkerStyle selects backtick-fenced markers (for .md) or plain markers (for other text files).
type MarkerStyle int

const (
	// MarkerStyleBacktick wraps conflict markers in Markdown inline-code backticks.
	MarkerStyleBacktick MarkerStyle = iota
	// MarkerStylePlain emits conflict markers without backticks for plain-text files.
	MarkerStylePlain
)

// Default backtick marker constants exported for .md conflict handling.
const (
	MarkerOpen  = "`<<<<< conflict`"
	MarkerClose = "`>>>>> conflict`"
	BaseLabel   = "base"

	PlainMarkerOpen  = "<<<<< conflict"
	PlainMarkerClose = ">>>>> conflict"
)

type markerSpec struct {
	open      string
	close     string
	segPrefix string
	segSuffix string
	segRe     *regexp.Regexp
	likeRe    *regexp.Regexp
}

var (
	backtickSpec = markerSpec{
		open:      MarkerOpen,
		close:     MarkerClose,
		segPrefix: "`===== ",
		segSuffix: "`",
		segRe:     regexp.MustCompile("^`===== (base|A\\d+)`$"),
		likeRe:    regexp.MustCompile("^`(<<<<< conflict|>>>>> conflict|===== (base|A\\d+))`$"),
	}
	plainSpec = markerSpec{
		open:      PlainMarkerOpen,
		close:     PlainMarkerClose,
		segPrefix: "===== ",
		segSuffix: "",
		segRe:     regexp.MustCompile("^===== (base|A\\d+)$"),
		likeRe:    regexp.MustCompile("^(<<<<< conflict|>>>>> conflict|===== (base|A\\d+))$"),
	}
)

func specForStyle(style MarkerStyle) markerSpec {
	if style == MarkerStylePlain {
		return plainSpec
	}
	return backtickSpec
}

// StyleForPath returns MarkerStyleBacktick for .md files and MarkerStylePlain otherwise.
func StyleForPath(p string) MarkerStyle {
	if strings.EqualFold(path.Ext(strings.TrimSpace(p)), ".md") {
		return MarkerStyleBacktick
	}
	return MarkerStylePlain
}

// IsTextMergePath reports whether p is a text extension eligible for N-way merge.
func IsTextMergePath(p string) bool {
	switch strings.ToLower(path.Ext(strings.TrimSpace(p))) {
	case ".md", ".txt", ".csv", ".tsv", ".yaml", ".yml", ".json", ".toml", ".css", ".html", ".xml", ".svg", ".tex":
		return true
	default:
		return false
	}
}

// SegmentMarker returns the formatted introducer line for a segment label (e.g., "`===== A1`").
func SegmentMarker(label string) string {
	return SegmentMarkerWith(label, MarkerStyleBacktick)
}

// SegmentMarkerWith returns the formatted introducer line for label in the given style.
func SegmentMarkerWith(label string, style MarkerStyle) string {
	sp := specForStyle(style)
	return sp.segPrefix + label + sp.segSuffix
}

// RegionSegment holds the lines belonging to base or one alternative (A1, A2, ...).
type RegionSegment struct {
	Label string
	Lines []string
}

// ConflictRegion holds the base segment and N >= 2 distinct alternatives for one conflicted span.
type ConflictRegion struct {
	Segments []RegionSegment
}

// Segment represents either a stable line run or a conflict region.
type Segment struct {
	Kind   string // "stable" or "region"
	Lines  []string
	Region *ConflictRegion
}

// ParsedFile represents a file decomposed into stable runs interleaved with conflict regions.
type ParsedFile struct {
	Segments []Segment
}

// ReconcileResult reports the outcome of ReconcileText / ReconcileVersions.
// Kind is "clean", "folded", or "keepBoth".
type ReconcileResult struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	Changed bool   `json:"changed"`
}

// HasConflictMarkers reports whether text contains a whole-line backtick conflict marker.
func HasConflictMarkers(text string) bool {
	return HasConflictMarkersWith(text, MarkerStyleBacktick)
}

// HasConflictMarkersWith reports whether text contains any whole-line conflict marker for style.
func HasConflictMarkersWith(text string, style MarkerStyle) bool {
	return looksLikeConflict(text, specForStyle(style))
}

func normalizeLF(text string) string {
	if !strings.Contains(text, "\r") {
		return text
	}
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

func splitLines(text string) []string {
	return strings.Split(normalizeLF(text), "\n")
}

func joinLines(lines []string) string {
	return strings.Join(lines, "\n")
}

func looksLikeConflict(text string, sp markerSpec) bool {
	return slices.ContainsFunc(splitLines(text), sp.likeRe.MatchString)
}

func validRegion(segs []RegionSegment) bool {
	baseCount := 0
	seen := make(map[string]bool, len(segs))
	for _, s := range segs {
		if seen[s.Label] {
			return false
		}
		seen[s.Label] = true
		if s.Label == BaseLabel {
			baseCount++
		}
	}
	altCount := len(segs) - baseCount
	return baseCount == 1 && altCount >= 2
}

func parseRegion(lines []string, start int, sp markerSpec) (*ConflictRegion, int, bool) {
	var segs []RegionSegment
	var current *RegionSegment

	for i := start; i < len(lines); i++ {
		line := lines[i]
		if line == sp.close {
			if current != nil {
				segs = append(segs, *current)
			}
			if !validRegion(segs) {
				return nil, 0, false
			}
			return &ConflictRegion{Segments: segs}, i + 1, true
		}
		if line == sp.open {
			return nil, 0, false
		}
		if m := sp.segRe.FindStringSubmatch(line); m != nil {
			if current != nil {
				segs = append(segs, *current)
			}
			current = &RegionSegment{Label: m[1], Lines: make([]string, 0)}
			continue
		}
		if sp.likeRe.MatchString(line) || current == nil {
			return nil, 0, false
		}
		current.Lines = append(current.Lines, line)
	}
	return nil, 0, false
}

// ParseConflictFile decomposes Markdown conflict text into stable runs and conflict regions.
func ParseConflictFile(text string) (*ParsedFile, bool) {
	return ParseConflictFileWith(text, MarkerStyleBacktick)
}

// ParseConflictFileWith decomposes text using the specified MarkerStyle.
func ParseConflictFileWith(text string, style MarkerStyle) (*ParsedFile, bool) {
	sp := specForStyle(style)
	lines := splitLines(text)
	var segments []Segment
	var stable []string
	flush := func() {
		if len(stable) > 0 {
			segments = append(segments, Segment{Kind: "stable", Lines: stable})
			stable = nil
		}
	}

	for i := 0; i < len(lines); {
		line := lines[i]
		if line == sp.open {
			flush()
			region, nextIdx, ok := parseRegion(lines, i+1, sp)
			if !ok {
				return nil, false
			}
			segments = append(segments, Segment{Kind: "region", Region: region})
			i = nextIdx
			continue
		}
		if sp.likeRe.MatchString(line) {
			return nil, false
		}
		stable = append(stable, line)
		i++
	}
	flush()
	return &ParsedFile{Segments: segments}, true
}

// EmitConflictFile serializes a ParsedFile back to Markdown-safe text.
func EmitConflictFile(parsed *ParsedFile) string {
	return EmitConflictFileWith(parsed, MarkerStyleBacktick)
}

// EmitConflictFileWith serializes a ParsedFile using the specified MarkerStyle.
func EmitConflictFileWith(parsed *ParsedFile, style MarkerStyle) string {
	sp := specForStyle(style)
	var out []string
	for _, seg := range parsed.Segments {
		if seg.Kind == "stable" {
			out = append(out, seg.Lines...)
			continue
		}
		out = append(out, sp.open)
		for _, s := range seg.Region.Segments {
			out = append(out, sp.segPrefix+s.Label+sp.segSuffix)
			out = append(out, s.Lines...)
		}
		out = append(out, sp.close)
	}
	return joinLines(out)
}

func regionsOf(p *ParsedFile) []*ConflictRegion {
	var out []*ConflictRegion
	for _, seg := range p.Segments {
		if seg.Kind == "region" {
			out = append(out, seg.Region)
		}
	}
	return out
}

func baseSegmentOf(r *ConflictRegion) []string {
	for _, s := range r.Segments {
		if s.Label == BaseLabel {
			return s.Lines
		}
	}
	return nil
}

func baseReconLines(p *ParsedFile) []string {
	var out []string
	for _, seg := range p.Segments {
		if seg.Kind == "stable" {
			out = append(out, seg.Lines...)
		} else {
			out = append(out, baseSegmentOf(seg.Region)...)
		}
	}
	return out
}

type baseRange struct {
	start int
	end   int
}

func regionRanges(p *ParsedFile) []baseRange {
	var ranges []baseRange
	idx := 0
	for _, seg := range p.Segments {
		if seg.Kind == "stable" {
			idx += len(seg.Lines)
			continue
		}
		length := len(baseSegmentOf(seg.Region))
		ranges = append(ranges, baseRange{start: idx, end: idx + length})
		idx += length
	}
	return ranges
}

func writeLenPrefixedLines(b *strings.Builder, lines []string) {
	b.WriteString(strconv.Itoa(len(lines)))
	b.WriteByte('#')
	for _, l := range lines {
		b.WriteString(strconv.Itoa(len(l)))
		b.WriteByte(':')
		b.WriteString(l)
		b.WriteByte(';')
	}
}

func skeletonOf(p *ParsedFile) string {
	var b strings.Builder
	for _, s := range p.Segments {
		if s.Kind == "stable" {
			b.WriteByte('S')
			writeLenPrefixedLines(&b, s.Lines)
		} else {
			b.WriteByte('R')
			writeLenPrefixedLines(&b, baseSegmentOf(s.Region))
		}
	}
	return b.String()
}

type contribVec = [][]string

func normKey(vec contribVec, regions []*ConflictRegion) string {
	var b strings.Builder
	for i, s := range vec {
		resolved := s
		if resolved == nil {
			resolved = baseSegmentOf(regions[i])
		}
		writeLenPrefixedLines(&b, resolved)
	}
	return b.String()
}

func contributionsOf(p *ParsedFile) ([]string, map[string]contribVec) {
	regions := regionsOf(p)
	m := make(map[string]contribVec)
	var order []string
	for ri, reg := range regions {
		for _, seg := range reg.Segments {
			if seg.Label == BaseLabel {
				continue
			}
			vec, ok := m[seg.Label]
			if !ok {
				vec = make(contribVec, len(regions))
				m[seg.Label] = vec
				order = append(order, seg.Label)
			}
			vec[ri] = seg.Lines
		}
	}
	return order, m
}

// expandParsedVersions reconstructs the full clean text for each alternative
// contribution in a ParsedFile.
func expandParsedVersions(p *ParsedFile) []string {
	regions := regionsOf(p)
	order, cmap := contributionsOf(p)
	out := make([]string, 0, len(order))
	for _, label := range order {
		vec := cmap[label]
		var lines []string
		ri := 0
		for _, seg := range p.Segments {
			if seg.Kind == "stable" {
				lines = append(lines, seg.Lines...)
				continue
			}
			if vec[ri] != nil {
				lines = append(lines, vec[ri]...)
			} else {
				lines = append(lines, baseSegmentOf(regions[ri])...)
			}
			ri++
		}
		out = append(out, joinLines(lines))
	}
	return out
}

func maxAltNumber(labels []string) int {
	maxVal := 0
	for _, l := range labels {
		if strings.HasPrefix(l, "A") {
			if n, err := strconv.Atoi(l[1:]); err == nil && n > maxVal {
				maxVal = n
			}
		}
	}
	return maxVal
}

type orderedContrib struct {
	label string
	vec   contribVec
}

func emitFromContributions(skeleton *ParsedFile, regions []*ConflictRegion, contribs []orderedContrib, style MarkerStyle) string {
	segments := make([]Segment, 0, len(skeleton.Segments))
	ri := 0
	for _, seg := range skeleton.Segments {
		if seg.Kind == "stable" {
			segments = append(segments, seg)
			continue
		}
		region := regions[ri]
		segs := []RegionSegment{{Label: BaseLabel, Lines: baseSegmentOf(region)}}
		for _, c := range contribs {
			if lines := c.vec[ri]; lines != nil {
				segs = append(segs, RegionSegment{Label: c.label, Lines: lines})
			}
		}
		segments = append(segments, Segment{Kind: "region", Region: &ConflictRegion{Segments: segs}})
		ri++
	}
	return EmitConflictFileWith(&ParsedFile{Segments: segments}, style)
}

func localizeClean(anchor *ParsedFile, base, cleanText string) (contribVec, bool) {
	recon := baseReconLines(anchor)
	if joinLines(recon) != base {
		return nil, false
	}
	cleanLines := splitLines(cleanText)
	ranges := regionRanges(anchor)
	blocks := DiffIndices(recon, cleanLines)

	for _, b := range blocks {
		inside := false
		for _, r := range ranges {
			if b.Buf1Start >= r.start && b.Buf1Start+b.Buf1Len <= r.end {
				inside = true
				break
			}
		}
		if !inside {
			return nil, false
		}
	}

	regions := regionsOf(anchor)
	slicesVec := make(contribVec, len(ranges))
	shift := 0
	bi := 0
	for ri, r := range ranges {
		for bi < len(blocks) && blocks[bi].Buf1Start+blocks[bi].Buf1Len < r.start {
			shift += blocks[bi].Buf2Len - blocks[bi].Buf1Len
			bi++
		}
		cStart := r.start + shift
		withinDelta := 0
		for bi < len(blocks) && blocks[bi].Buf1Start >= r.start && blocks[bi].Buf1Start+blocks[bi].Buf1Len <= r.end {
			withinDelta += blocks[bi].Buf2Len - blocks[bi].Buf1Len
			bi++
		}
		shift += withinDelta
		slice := cleanLines[cStart : r.end+shift]
		if slices.Equal(slice, baseSegmentOf(regions[ri])) {
			slicesVec[ri] = nil
		} else {
			slicesVec[ri] = slice
		}
	}
	return slicesVec, true
}

func mint(base, local, remote string, style MarkerStyle) ReconcileResult {
	return mintMulti(splitLines(base), [][]string{splitLines(local), splitLines(remote)}, false, style)
}

// mintMulti simultaneously merges N >= 2 clean line slices against baseLines.
// When emptyBase is true (2-way mode without an external base revision), conflict
// regions emit an empty base segment and retain all distinct version slices.
func mintMulti(baseLines []string, versions [][]string, emptyBase bool, style MarkerStyle) ReconcileResult {
	k := len(versions)
	hunks := make([][]DiffBlock, k)
	for m := range k {
		hunks[m] = DiffIndices(baseLines, versions[m])
	}

	idx := make([]int, k)
	delta := make([]int, k)
	startPos := make([]int, k)
	used := make([]bool, k)
	basePos := 0

	var segments []Segment
	var stable []string
	conflicted := false
	flush := func() {
		if len(stable) > 0 {
			segments = append(segments, Segment{Kind: "stable", Lines: stable})
			stable = nil
		}
	}

	for {
		bestM := -1
		var bestStart, bestLen int
		for m := range k {
			if idx[m] >= len(hunks[m]) {
				continue
			}
			hk := hunks[m][idx[m]]
			if bestM == -1 || hk.Buf1Start < bestStart || (hk.Buf1Start == bestStart && hk.Buf1Len > bestLen) {
				bestM = m
				bestStart = hk.Buf1Start
				bestLen = hk.Buf1Len
			}
		}
		if bestM == -1 {
			break
		}

		spanStart := bestStart
		spanEnd := spanStart + bestLen
		if spanStart > basePos {
			stable = append(stable, slices.Clone(baseLines[basePos:spanStart])...)
		}

		for m := range k {
			startPos[m] = spanStart + delta[m]
			used[m] = false
		}

		seed := hunks[bestM][idx[bestM]]
		used[bestM] = true
		delta[bestM] += seed.Buf2Len - seed.Buf1Len
		idx[bestM]++

		for {
			expanded := false
			for m := range k {
				for idx[m] < len(hunks[m]) {
					hk := hunks[m][idx[m]]
					hStart := hk.Buf1Start
					hEnd := hStart + hk.Buf1Len
					if !hunksOverlap(spanStart, spanEnd, hStart, hEnd) {
						break
					}
					used[m] = true
					if hEnd > spanEnd {
						spanEnd = hEnd
						expanded = true
					}
					delta[m] += hk.Buf2Len - hk.Buf1Len
					idx[m]++
				}
			}
			if !expanded {
				break
			}
		}

		oSlice := baseLines[spanStart:spanEnd]
		var alts [][]string
		for m := range k {
			vSlice := versions[m][startPos[m] : spanEnd+delta[m]]
			if !emptyBase && (!used[m] || slices.Equal(vSlice, oSlice)) {
				continue
			}
			dup := false
			for _, existing := range alts {
				if slices.Equal(existing, vSlice) {
					dup = true
					break
				}
			}
			if !dup {
				alts = append(alts, slices.Clone(vSlice))
			}
		}

		if !emptyBase && len(alts) == 0 {
			stable = append(stable, slices.Clone(oSlice)...)
		} else if len(alts) == 1 {
			stable = append(stable, alts[0]...)
		} else {
			conflicted = true
			flush()
			baseSeg := []string{}
			if !emptyBase {
				baseSeg = nonNilSlice(slices.Clone(oSlice))
			}
			regSegs := make([]RegionSegment, 0, len(alts)+1)
			regSegs = append(regSegs, RegionSegment{Label: BaseLabel, Lines: baseSeg})
			for j, alt := range alts {
				regSegs = append(regSegs, RegionSegment{
					Label: fmt.Sprintf("A%d", j+1),
					Lines: nonNilSlice(alt),
				})
			}
			segments = append(segments, Segment{
				Kind:   "region",
				Region: &ConflictRegion{Segments: regSegs},
			})
		}

		basePos = spanEnd
	}

	if basePos < len(baseLines) {
		stable = append(stable, slices.Clone(baseLines[basePos:])...)
	}
	flush()

	content := EmitConflictFileWith(&ParsedFile{Segments: segments}, style)
	lastRemote := joinLines(versions[k-1])
	if conflicted {
		return ReconcileResult{Kind: "folded", Changed: content != lastRemote, Content: content}
	}
	return ReconcileResult{Kind: "clean", Changed: content != lastRemote, Content: content}
}

// mintTwoWay builds minimal whole-line conflict regions when no common base exists.
func mintTwoWay(local, remote string, style MarkerStyle) ReconcileResult {
	linesA := splitLines(local)
	linesB := splitLines(remote)
	common := commonLCSLines([][]string{linesA, linesB})
	res := mintMulti(common, [][]string{linesA, linesB}, true, style)
	if res.Kind == "folded" {
		res.Changed = true
	}
	return res
}

func commonLCSLines(versions [][]string) []string {
	if len(versions) == 0 {
		return nil
	}
	common := slices.Clone(versions[0])
	for _, v := range versions[1:] {
		blocks := DiffIndices(common, v)
		if len(blocks) == 0 {
			continue
		}
		var next []string
		cur := 0
		for _, blk := range blocks {
			if blk.Buf1Start > cur {
				next = append(next, common[cur:blk.Buf1Start]...)
			}
			cur = blk.Buf1Start + blk.Buf1Len
		}
		if cur < len(common) {
			next = append(next, common[cur:]...)
		}
		common = next
	}
	return common
}

func nonNilSlice(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func foldInto(anchor *ParsedFile, anchorText, base string, incoming []contribVec, style MarkerStyle) ReconcileResult {
	if joinLines(baseReconLines(anchor)) != base {
		return ReconcileResult{Kind: "keepBoth"}
	}
	regions := regionsOf(anchor)
	order, cmap := contributionsOf(anchor)

	seen := make(map[string]struct{}, len(cmap))
	for _, vec := range cmap {
		seen[normKey(vec, regions)] = struct{}{}
	}
	baseKey := normKey(make(contribVec, len(regions)), regions)

	var fresh []contribVec
	for _, vec := range incoming {
		key := normKey(vec, regions)
		if key == baseKey {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		fresh = append(fresh, vec)
	}
	if len(fresh) == 0 {
		return ReconcileResult{Kind: "folded", Changed: false, Content: anchorText}
	}

	n := maxAltNumber(order)
	contribs := make([]orderedContrib, 0, len(order)+len(fresh))
	for _, label := range order {
		contribs = append(contribs, orderedContrib{label: label, vec: cmap[label]})
	}
	for _, vec := range fresh {
		n++
		contribs = append(contribs, orderedContrib{label: fmt.Sprintf("A%d", n), vec: vec})
	}
	return ReconcileResult{
		Kind:    "folded",
		Changed: true,
		Content: emitFromContributions(anchor, regions, contribs, style),
	}
}

func asResult(content string, push bool, style MarkerStyle) ReconcileResult {
	sp := specForStyle(style)
	if !looksLikeConflict(content, sp) {
		return ReconcileResult{Kind: "clean", Changed: push, Content: content}
	}
	if _, ok := ParseConflictFileWith(content, style); !ok {
		return ReconcileResult{Kind: "keepBoth"}
	}
	return ReconcileResult{Kind: "folded", Changed: push, Content: content}
}

// ReconcileText reconciles local and remote Markdown text against base.
func ReconcileText(base, local, remote string) ReconcileResult {
	return ReconcileTextWith(base, local, remote, MarkerStyleBacktick)
}

// ReconcileTextWith reconciles local and remote text against base using style.
func ReconcileTextWith(base, local, remote string, style MarkerStyle) ReconcileResult {
	base = normalizeLF(base)
	local = normalizeLF(local)
	remote = normalizeLF(remote)
	sp := specForStyle(style)
	if local == remote {
		return asResult(local, false, style)
	}
	if local == base {
		return asResult(remote, false, style)
	}
	if remote == base {
		return asResult(local, true, style)
	}

	cleanBase := base
	if looksLikeConflict(base, sp) {
		bp, ok := ParseConflictFileWith(base, style)
		if !ok {
			return ReconcileResult{Kind: "keepBoth"}
		}
		cleanBase = joinLines(baseReconLines(bp))
	}

	localMarked := looksLikeConflict(local, sp)
	remoteMarked := looksLikeConflict(remote, sp)

	if !localMarked && !remoteMarked {
		return mint(cleanBase, local, remote, style)
	}

	var localParsed, remoteParsed *ParsedFile
	if localMarked {
		var ok bool
		localParsed, ok = ParseConflictFileWith(local, style)
		if !ok {
			return ReconcileResult{Kind: "keepBoth"}
		}
	}
	if remoteMarked {
		var ok bool
		remoteParsed, ok = ParseConflictFileWith(remote, style)
		if !ok {
			return ReconcileResult{Kind: "keepBoth"}
		}
	}

	if !localMarked && remoteParsed != nil {
		if vec, ok := localizeClean(remoteParsed, cleanBase, local); ok {
			return foldInto(remoteParsed, remote, cleanBase, []contribVec{vec}, style)
		}
		expanded := append(expandParsedVersions(remoteParsed), local)
		return reconcileExpandedClean(cleanBase, true, expanded, remote, style)
	}

	if localParsed != nil && !remoteMarked {
		if vec, ok := localizeClean(localParsed, cleanBase, remote); ok {
			r := foldInto(localParsed, local, cleanBase, []contribVec{vec}, style)
			if r.Kind == "folded" {
				r.Changed = true
			}
			return r
		}
		expanded := append(expandParsedVersions(localParsed), remote)
		return reconcileExpandedClean(cleanBase, true, expanded, remote, style)
	}

	if localParsed != nil && remoteParsed != nil {
		if skeletonOf(localParsed) == skeletonOf(remoteParsed) {
			lOrder, lMap := contributionsOf(localParsed)
			incoming := make([]contribVec, 0, len(lOrder))
			for _, label := range lOrder {
				incoming = append(incoming, lMap[label])
			}
			if r := foldInto(remoteParsed, remote, cleanBase, incoming, style); r.Kind != "keepBoth" {
				return r
			}
		}
		expanded := append(expandParsedVersions(remoteParsed), expandParsedVersions(localParsed)...)
		return reconcileExpandedClean(cleanBase, true, expanded, remote, style)
	}

	return ReconcileResult{Kind: "keepBoth"}
}

func reconcileExpandedClean(cleanBase string, hasBase bool, versions []string, compareRemote string, style MarkerStyle) ReconcileResult {
	seen := make(map[string]bool, len(versions))
	var lineSlices [][]string
	for _, v := range versions {
		if !seen[v] {
			seen[v] = true
			lineSlices = append(lineSlices, splitLines(v))
		}
	}
	if len(lineSlices) == 0 {
		return ReconcileResult{Kind: "clean", Content: ""}
	}
	if len(lineSlices) == 1 {
		c := joinLines(lineSlices[0])
		return asResult(c, c != compareRemote, style)
	}
	baseLines := splitLines(cleanBase)
	if !hasBase {
		baseLines = commonLCSLines(lineSlices)
	}
	res := mintMulti(baseLines, lineSlices, !hasBase, style)
	res.Changed = res.Content != compareRemote
	return res
}

// ReconcileVersions folds N distinct versions (e.g. local + 1..K Drive duplicates).
// When hasBase is false, falls back to minimal 2-way diffing across versions.
func ReconcileVersions(base string, hasBase bool, versions []string, style MarkerStyle) ReconcileResult {
	base = normalizeLF(base)
	seen := make(map[string]bool, len(versions))
	unique := make([]string, 0, len(versions))
	for _, v := range versions {
		norm := normalizeLF(v)
		if !seen[norm] {
			seen[norm] = true
			unique = append(unique, norm)
		}
	}
	if len(unique) == 0 {
		return ReconcileResult{Kind: "clean", Content: ""}
	}
	if len(unique) == 1 {
		return asResult(unique[0], false, style)
	}

	sp := specForStyle(style)
	cleanBase := base
	if hasBase && looksLikeConflict(base, sp) {
		bp, ok := ParseConflictFileWith(base, style)
		if !ok {
			return ReconcileResult{Kind: "keepBoth"}
		}
		cleanBase = joinLines(baseReconLines(bp))
	}

	var expanded []string
	anyMarked := false
	for _, v := range unique {
		if looksLikeConflict(v, sp) {
			anyMarked = true
			parsed, ok := ParseConflictFileWith(v, style)
			if !ok {
				return ReconcileResult{Kind: "keepBoth"}
			}
			expanded = append(expanded, expandParsedVersions(parsed)...)
		} else {
			expanded = append(expanded, v)
		}
	}

	// Fast path when iterative folding along identical region skeletons applies cleanly.
	if hasBase && anyMarked {
		acc := unique[0]
		okAll := true
		for _, v := range unique[1:] {
			step := ReconcileTextWith(cleanBase, acc, v, style)
			if step.Kind == "keepBoth" {
				okAll = false
				break
			}
			acc = step.Content
		}
		if okAll {
			return asResult(acc, true, style)
		}
	}

	res := reconcileExpandedClean(cleanBase, hasBase, expanded, "", style)
	if res.Kind != "keepBoth" {
		res.Changed = true
	}
	return res
}
