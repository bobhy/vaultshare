package merge

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestReconcileTextCleanAndShortCircuits(t *testing.T) {
	tests := []struct {
		name        string
		base        string
		local       string
		remote      string
		wantKind    string
		wantChanged bool
		wantContent string
	}{
		{
			name:        "local equals remote no-op",
			base:        "a\nb\nc",
			local:       "a\nx\nc",
			remote:      "a\nx\nc",
			wantKind:    "clean",
			wantChanged: false,
			wantContent: "a\nx\nc",
		},
		{
			name:        "local unchanged takes remote without pushing",
			base:        "a\nb\nc",
			local:       "a\nb\nc",
			remote:      "a\nb\nd",
			wantKind:    "clean",
			wantChanged: false,
			wantContent: "a\nb\nd",
		},
		{
			name:        "remote unchanged pushes local",
			base:        "a\nb\nc",
			local:       "x\nb\nc",
			remote:      "a\nb\nc",
			wantKind:    "clean",
			wantChanged: true,
			wantContent: "x\nb\nc",
		},
		{
			name:        "disjoint edits merge cleanly",
			base:        "line 1\nline 2\nline 3\nline 4",
			local:       "local 1\nline 2\nline 3\nline 4",
			remote:      "line 1\nline 2\nline 3\nremote 4",
			wantKind:    "clean",
			wantChanged: true,
			wantContent: "local 1\nline 2\nline 3\nremote 4",
		},
		{
			name:        "deletion and disjoint edit merge cleanly",
			base:        "one\ntwo\nthree\nfour",
			local:       "one\nthree\nfour",
			remote:      "one\ntwo\nthree\nfour-updated",
			wantKind:    "clean",
			wantChanged: true,
			wantContent: "one\nthree\nfour-updated",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ReconcileText(tc.base, tc.local, tc.remote)
			if got.Kind != tc.wantKind || got.Changed != tc.wantChanged || got.Content != tc.wantContent {
				t.Fatalf("ReconcileText() = %+v, want kind=%q changed=%v content=%q", got, tc.wantKind, tc.wantChanged, tc.wantContent)
			}
		})
	}
}

func TestReconcileVersionsNWayAndPlainMarkers(t *testing.T) {
	base := "header\noriginal\nfooter"
	v1 := "header\nv1\nfooter"
	v2 := "header\nv2\nfooter"
	v3 := "header\nv3\nfooter"

	mdRes := ReconcileVersions(base, true, []string{v1, v2, v3}, StyleForPath("Notes/Test.md"))
	if mdRes.Kind != "folded" {
		t.Fatalf("expected folded markdown, got %+v", mdRes)
	}
	if !strings.Contains(mdRes.Content, MarkerOpen) || !strings.Contains(mdRes.Content, "`===== A3`") {
		t.Fatalf("expected backtick A3 marker in:\n%s", mdRes.Content)
	}

	txtRes := ReconcileVersions(base, true, []string{v1, v2}, StyleForPath("Notes/Test.txt"))
	if txtRes.Kind != "folded" {
		t.Fatalf("expected folded plain text, got %+v", txtRes)
	}
	if !strings.Contains(txtRes.Content, PlainMarkerOpen) || strings.Contains(txtRes.Content, "`<<<<<") {
		t.Fatalf("expected plain markers without backticks in:\n%s", txtRes.Content)
	}
	if parsed, ok := ParseConflictFileWith(txtRes.Content, MarkerStylePlain); !ok || EmitConflictFileWith(parsed, MarkerStylePlain) != txtRes.Content {
		t.Fatalf("plain marker round-trip failed:\n%s", txtRes.Content)
	}

	// 2-way fallback without base keeps matching prefix/suffix lines outside conflict region.
	twoWay := ReconcileVersions("", false, []string{v1, v2}, MarkerStyleBacktick)
	if twoWay.Kind != "folded" {
		t.Fatalf("expected folded 2-way result, got %+v", twoWay)
	}
	if !strings.HasPrefix(twoWay.Content, "header\n"+MarkerOpen) || !strings.HasSuffix(twoWay.Content, MarkerClose+"\nfooter") {
		t.Fatalf("2-way fallback wrapped stable lines:\n%s", twoWay.Content)
	}
}

func TestDiff3MergeInsertionAtEditAndDeleteBoundary(t *testing.T) {
	t.Run("multi-line delete vs insertion at same start index does not panic and is symmetric", func(t *testing.T) {
		o := []string{"L0", "L1", "L2", "L3", "L4"}
		a := []string{"L0", "L4"}
		b := []string{"L0", "INS", "L1", "L2", "L3", "L4"}

		regAB := Diff3Merge(a, o, b)
		regBA := Diff3Merge(b, o, a)
		if len(regAB) != 3 || regAB[1].Conflict == nil {
			t.Fatalf("expected [OK(L0), Conflict, OK(L4)], got %+v", regAB)
		}
		if len(regBA) != 3 || regBA[1].Conflict == nil {
			t.Fatalf("expected symmetric [OK(L0), Conflict, OK(L4)], got %+v", regBA)
		}
		if !slices.Equal(regAB[1].Conflict.O, []string{"L1", "L2", "L3"}) ||
			!slices.Equal(regAB[1].Conflict.A, []string{}) ||
			!slices.Equal(regAB[1].Conflict.B, []string{"INS", "L1", "L2", "L3"}) {
			t.Fatalf("unexpected conflict hunk AB: %+v", regAB[1].Conflict)
		}
		if !slices.Equal(regBA[1].Conflict.O, regAB[1].Conflict.O) ||
			!slices.Equal(regBA[1].Conflict.A, regAB[1].Conflict.B) ||
			!slices.Equal(regBA[1].Conflict.B, regAB[1].Conflict.A) {
			t.Fatalf("asymmetric conflict hunks: AB=%+v BA=%+v", regAB[1].Conflict, regBA[1].Conflict)
		}
	})

	t.Run("single-line edit vs insertion at same start index conflicts instead of duplicating base", func(t *testing.T) {
		o := []string{"L0", "L1", "L2"}
		a := []string{"L0", "L1_mod", "L2"}
		b := []string{"L0", "INS", "L1", "L2"}

		resAB := ReconcileText(joinLines(o), joinLines(a), joinLines(b))
		resBA := ReconcileText(joinLines(o), joinLines(b), joinLines(a))
		if resAB.Kind != "folded" || resBA.Kind != "folded" {
			t.Fatalf("expected folded conflict in both orders, got AB=%+v BA=%+v", resAB, resBA)
		}
		if !strings.Contains(resAB.Content, "L1_mod") || !strings.Contains(resAB.Content, "INS\nL1") {
			t.Fatalf("missing expected alternatives in:\n%s", resAB.Content)
		}
	})
}

func TestReconcileVersionsThreeWayAndTwoWayInsertions(t *testing.T) {
	t.Run("3 versions without base preserves all 3 alternatives", func(t *testing.T) {
		res := ReconcileVersions("", false, []string{
			"header\nA\nfooter",
			"header\nB\nfooter",
			"header\nC\nfooter",
		}, MarkerStyleBacktick)
		if res.Kind != "folded" {
			t.Fatalf("expected folded, got %+v", res)
		}
		for _, want := range []string{"`===== A1`\nA", "`===== A2`\nB", "`===== A3`\nC"} {
			if !strings.Contains(res.Content, want) {
				t.Fatalf("missing %q in:\n%s", want, res.Content)
			}
		}
	})

	t.Run("3 versions with pure-insertion base preserves all 3 alternatives", func(t *testing.T) {
		res := ReconcileVersions("header\nfooter", true, []string{
			"header\nA\nfooter",
			"header\nB\nfooter",
			"header\nC\nfooter",
		}, MarkerStyleBacktick)
		if res.Kind != "folded" {
			t.Fatalf("expected folded, got %+v", res)
		}
		for _, want := range []string{"`===== A1`\nA", "`===== A2`\nB", "`===== A3`\nC"} {
			if !strings.Contains(res.Content, want) {
				t.Fatalf("missing %q in:\n%s", want, res.Content)
			}
		}
	})

	t.Run("ReconcileText folds 3rd insertion into existing empty-base conflict", func(t *testing.T) {
		base := "header\nfooter"
		step1 := ReconcileText(base, "header\nA\nfooter", "header\nB\nfooter")
		if step1.Kind != "folded" {
			t.Fatalf("expected step1 folded, got %+v", step1)
		}
		step2 := ReconcileText(base, step1.Content, "header\nC\nfooter")
		if step2.Kind != "folded" || !strings.Contains(step2.Content, "`===== A3`\nC") {
			t.Fatalf("expected A3 folded into empty-base region, got:\n%s", step2.Content)
		}
	})
}

func TestReconcileVersionsDisjointCleanEditsAndMultiRegion(t *testing.T) {
	base := "line 1\nline 2\nline 3\nline 4"
	v1 := "v1_line1\nline 2\nline 3\nline 4"
	v2 := "v2_line1\nline 2\nline 3\nline 4"
	v3 := "line 1\nline 2\nline 3\nv3_line4"

	for _, order := range [][]string{
		{v1, v2, v3},
		{v1, v3, v2},
		{v3, v1, v2},
	} {
		res := ReconcileVersions(base, true, order, MarkerStyleBacktick)
		if res.Kind != "folded" {
			t.Fatalf("expected folded for order %v, got %+v", order, res)
		}
		if !strings.Contains(res.Content, "v1_line1") ||
			!strings.Contains(res.Content, "v2_line1") ||
			!strings.HasSuffix(res.Content, "\nline 2\nline 3\nv3_line4") {
			t.Fatalf("expected conflict on line 1 + clean edit on line 4, got:\n%s", res.Content)
		}
	}

	// Multiple distinct conflict regions across 3 versions.
	v4 := "line 1\nline 2\nline 3\nv4_line4"
	multi := ReconcileVersions(base, true, []string{v1, v2, v3, v4}, MarkerStyleBacktick)
	if multi.Kind != "folded" {
		t.Fatalf("expected folded multi-region result, got %+v", multi)
	}
	parsed, ok := ParseConflictFile(multi.Content)
	if !ok || len(regionsOf(parsed)) != 2 {
		t.Fatalf("expected 2 conflict regions, got ok=%v content:\n%s", ok, multi.Content)
	}
}

func TestPlainTextDividersAndInlineBacktickProse(t *testing.T) {
	t.Run("plain text files with ===== dividers merge cleanly", func(t *testing.T) {
		base := "Title\n================\nBody line 1\nBody line 2"
		local := "Title\n================\nBody line 1 edited\nBody line 2"
		remote := "Title\n================\nBody line 1\nBody line 2 edited"

		res := ReconcileTextWith(base, local, remote, MarkerStylePlain)
		if res.Kind != "clean" {
			t.Fatalf("expected clean merge on plain text with ===== divider, got %+v", res)
		}
		want := "Title\n================\nBody line 1 edited\nBody line 2 edited"
		if res.Content != want {
			t.Fatalf("got:\n%s\nwant:\n%s", res.Content, want)
		}
		if HasConflictMarkersWith(res.Content, MarkerStylePlain) {
			t.Fatalf("plain divider falsely detected as conflict marker")
		}
	})

	t.Run("inline backtick marker mentions in prose do not trigger conflict detection", func(t *testing.T) {
		prose := "Notes about VaultShare:\nInline mention of `<<<<< conflict` and `>>>>> conflict` in a sentence.\nEnd."
		if HasConflictMarkers(prose) {
			t.Fatalf("inline prose mention falsely flagged by HasConflictMarkers")
		}
		res := ReconcileText(prose, prose+"\nAdded local", prose)
		if res.Kind != "clean" {
			t.Fatalf("expected clean merge for prose mentioning markers inline, got %+v", res)
		}
	})

	t.Run("orphan whole-line marker is still flagged and rejected by parser", func(t *testing.T) {
		corrupt := "Header\n`<<<<< conflict`\n Orphaned body"
		if !HasConflictMarkers(corrupt) {
			t.Fatalf("expected orphan whole-line marker to be detected")
		}
		if _, ok := ParseConflictFile(corrupt); ok {
			t.Fatalf("expected ParseConflictFile to reject unclosed marker")
		}
	})
}

func TestCRLFNormalizationDuplicateLabelsAndNonUTF8(t *testing.T) {
	t.Run("CRLF and LF cross-platform disjoint edits merge cleanly", func(t *testing.T) {
		base := "a\r\nb\r\nc"
		local := "a_mod\nb\nc"
		remote := "a\r\nb\r\nc_mod"

		res := ReconcileText(base, local, remote)
		if res.Kind != "clean" || res.Content != "a_mod\nb\nc_mod" {
			t.Fatalf("expected clean cross-platform CRLF/LF merge, got %+v", res)
		}
	})

	t.Run("duplicate alternative labels in a conflict block are rejected", func(t *testing.T) {
		dup := strings.Join([]string{
			MarkerOpen,
			"`===== base`",
			"orig",
			"`===== A1`",
			"first",
			"`===== A1`",
			"second",
			MarkerClose,
		}, "\n")
		if _, ok := ParseConflictFile(dup); ok {
			t.Fatalf("expected ParseConflictFile to reject duplicate A1 labels")
		}
	})

	t.Run("distinct invalid UTF-8 byte sequences do not collide in normKey", func(t *testing.T) {
		base := "header\norig\nfooter"
		v1 := "header\nbyte-\xe8\nfooter"
		v2 := "header\nbyte-\xe9\nfooter"
		v3 := "header\nbyte-\xea\nfooter"

		res := ReconcileVersions(base, true, []string{v1, v2, v3}, MarkerStyleBacktick)
		if res.Kind != "folded" || !strings.Contains(res.Content, "`===== A3`") {
			t.Fatalf("expected all 3 non-UTF-8 alternatives preserved, got:\n%q", res.Content)
		}
	})
}

func TestLargeFileDiffMemoryAndSpeed(t *testing.T) {
	const totalLines = 10_000
	baseLines := make([]string, totalLines)
	for i := range totalLines {
		baseLines[i] = fmt.Sprintf("line-%05d: original content", i)
	}
	localLines := slices.Clone(baseLines)
	remoteLines := slices.Clone(baseLines)

	localLines[5] = "line-00005: local edit near top"
	localLines[5000] = "line-05000: local edit in middle"
	remoteLines[9990] = "line-09990: remote edit near bottom"

	res := ReconcileText(joinLines(baseLines), joinLines(localLines), joinLines(remoteLines))
	if res.Kind != "clean" {
		t.Fatalf("expected clean merge on 10,000-line file, got kind=%s", res.Kind)
	}
	outLines := splitLines(res.Content)
	if len(outLines) != totalLines ||
		outLines[5] != localLines[5] ||
		outLines[5000] != localLines[5000] ||
		outLines[9990] != remoteLines[9990] {
		t.Fatalf("unexpected merged lines on 10,000-line file (len=%d)", len(outLines))
	}

	// Exercise Hirschberg fallback on a 1,000-line repetitive un-anchorable span (1M cells > maxExactDPCells).
	repA := make([]string, 1000)
	repB := make([]string, 1000)
	for i := range 1000 {
		repA[i] = fmt.Sprintf("rep-%d", i%7)
		repB[i] = fmt.Sprintf("rep-%d", (i+1)%7)
	}
	blocks := DiffIndices(repA, repB)
	if len(blocks) == 0 {
		t.Fatalf("expected non-empty diff blocks on repetitive span")
	}
}
