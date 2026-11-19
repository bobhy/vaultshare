package merge

import (
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
