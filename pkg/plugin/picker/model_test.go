package picker

import (
	"strings"
	"testing"
)

func items(labels ...string) []Item {
	out := make([]Item, 0, len(labels))
	for _, l := range labels {
		out = append(out, Item{Label: l, Columns: []string{l}, Value: l})
	}
	return out
}

func TestFilterNarrowsAndClampsCursor(t *testing.T) {
	m := newModel(items("alpha", "beta", "gamma"))
	m.cursor = 2

	for _, r := range "bet" {
		m.push(r)
	}
	if got := len(m.visible()); got != 1 {
		t.Fatalf("visible() = %d rows, want 1", got)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d after the list shrank, want 0", m.cursor)
	}
	sel, ok := m.selected()
	if !ok || sel.Label != "beta" {
		t.Errorf("selected() = %q (%v), want beta", sel.Label, ok)
	}
}

func TestSelectedIsFalseWhenNothingMatches(t *testing.T) {
	m := newModel(items("alpha", "beta"))
	for _, r := range "zzz" {
		m.push(r)
	}
	if _, ok := m.selected(); ok {
		t.Error("selected() reported a row from an empty list")
	}
	// Enter must resolve to nothing rather than to a stale row.
	if got := m.confirmed(); len(got) != 0 {
		t.Errorf("confirmed() = %d rows on an empty list, want 0", len(got))
	}
}

func TestBackspaceAndClearRestoreTheFullList(t *testing.T) {
	m := newModel(items("alpha", "beta", "gamma"))
	m.push('b')
	if len(m.visible()) != 1 {
		t.Fatalf("filter did not narrow: %d rows", len(m.visible()))
	}
	m.backspace()
	if len(m.visible()) != 3 {
		t.Errorf("backspace left %d rows, want 3", len(m.visible()))
	}
	m.push('b')
	m.clear()
	if len(m.visible()) != 3 || m.query != "" {
		t.Errorf("clear left query %q and %d rows, want empty and 3", m.query, len(m.visible()))
	}
}

func TestCursorStaysInRange(t *testing.T) {
	m := newModel(items("a", "b", "c"))
	m.up()
	if m.cursor != 0 {
		t.Errorf("up() from the top moved the cursor to %d", m.cursor)
	}
	m.pageDown()
	if m.cursor != 2 {
		t.Errorf("pageDown() left the cursor at %d, want the last row (2)", m.cursor)
	}
	m.down()
	if m.cursor != 2 {
		t.Errorf("down() past the end moved the cursor to %d", m.cursor)
	}
}

// Enter with nothing marked is the single-item path: it must resolve to the
// row under the cursor so choosing one cluster costs no extra keystrokes.
func TestConfirmedFallsBackToCursorRow(t *testing.T) {
	m := newModel(items("a", "b", "c")).withMulti()
	m.down()
	got := m.confirmed()
	if len(got) != 1 || got[0].Label != "b" {
		t.Fatalf("confirmed() = %v, want [b]", labelsOf(got))
	}
}

func TestToggleMarksAndAdvances(t *testing.T) {
	m := newModel(items("a", "b", "c")).withMulti()
	m.toggle() // marks a, moves to b
	m.toggle() // marks b, moves to c
	if m.markedCount() != 2 {
		t.Fatalf("markedCount() = %d, want 2", m.markedCount())
	}
	got := m.confirmed()
	if len(got) != 2 || got[0].Label != "a" || got[1].Label != "b" {
		t.Errorf("confirmed() = %v, want [a b]", labelsOf(got))
	}
}

func TestToggleIsReversible(t *testing.T) {
	m := newModel(items("a", "b")).withMulti()
	m.toggle()
	m.cursor = 0
	m.toggle()
	if m.markedCount() != 0 {
		t.Errorf("markedCount() = %d after toggling the same row twice, want 0", m.markedCount())
	}
}

// The fleet path: filter to a subset, mark all of it, clear the filter. The
// marks must survive refiltering, and must cover only what was shown.
func TestToggleAllMarksOnlyVisibleRowsAndSurvivesRefilter(t *testing.T) {
	m := newModel(items("prod-a", "prod-b", "test-c")).withMulti()
	for _, r := range "prod" {
		m.push(r)
	}
	m.toggleAll()
	m.clear()

	got := m.confirmed()
	if len(got) != 2 {
		t.Fatalf("confirmed() = %v, want the two prod rows", labelsOf(got))
	}
	for _, it := range got {
		if it.Label == "test-c" {
			t.Errorf("confirmed() included %q, which the filter had hidden", it.Label)
		}
	}
}

func TestToggleAllClearsWhenEverythingShownIsMarked(t *testing.T) {
	m := newModel(items("a", "b")).withMulti()
	m.toggleAll()
	if m.markedCount() != 2 {
		t.Fatalf("markedCount() = %d after the first Ctrl-A, want 2", m.markedCount())
	}
	m.toggleAll()
	if m.markedCount() != 0 {
		t.Errorf("markedCount() = %d after the second Ctrl-A, want 0", m.markedCount())
	}
}

func TestMarkingIsInertInSingleSelectMode(t *testing.T) {
	m := newModel(items("a", "b"))
	m.toggle()
	m.toggleAll()
	if m.markedCount() != 0 {
		t.Errorf("markedCount() = %d in single-select mode, want 0", m.markedCount())
	}
}

func TestRowsRenderMarkerGutterAlignedWithHeader(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"alpha", "1"}},
		{Label: "b", Columns: []string{"b", "2"}},
	}).withHeader(Item{Columns: []string{"NAME", "N"}}).withMulti()
	m.toggle()

	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows() = %d, want 2", len(rows))
	}
	if rows[0][:4] != "[x] " {
		t.Errorf("marked row starts %q, want %q", rows[0][:4], "[x] ")
	}
	if rows[1][:4] != "[ ] " {
		t.Errorf("unmarked row starts %q, want %q", rows[1][:4], "[ ] ")
	}
	header := m.headerRow()
	if header[:markerWidth] != "    " {
		t.Errorf("header %q is not indented by the marker gutter", header)
	}
	// The columns themselves must still line up under the header.
	if len(rows[0]) != len(rows[1]) {
		t.Errorf("rows are not padded to equal width: %q vs %q", rows[0], rows[1])
	}
}

func TestRenderRowHasNoTrailingWhitespace(t *testing.T) {
	got := renderRow(Item{Columns: []string{"a", "bb"}}, []int{5, 5})
	if got != "a      bb" {
		t.Errorf("renderRow() = %q", got)
	}
}

func labelsOf(items []Item) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Label)
	}
	return out
}

// kindItems is the kubevirt-derived fixture: a fixed set of Kubernetes-kind-
// shaped rows, used by the tests merged in below from vitictl-kubevirt.
func kindItems() []Item {
	return []Item{
		{Label: "kubernetescluster", Columns: []string{"kubernetescluster", "kubernetesclusters"}},
		{Label: "pod", Columns: []string{"pod", "pods"}},
		{Label: "vulnerabilityreport", Columns: []string{"vulnerabilityreport", "vulnerabilityreports"}},
		{Label: "namespace", Columns: []string{"namespace", "namespaces"}},
	}
}

func visibleKindLabels(m *model) []string {
	out := make([]string, 0, len(m.visible()))
	for _, it := range m.visible() {
		out = append(out, it.Label)
	}
	return out
}

func TestNewModelShowsEverythingWithTheFirstRowSelected(t *testing.T) {
	m := newModel(kindItems())

	if got := len(m.visible()); got != 4 {
		t.Errorf("visible() = %d items, want all 4", got)
	}
	sel, ok := m.selected()
	if !ok {
		t.Fatal("selected() = not ok, want the first row")
	}
	if sel.Label != "kubernetescluster" {
		t.Errorf("selected() = %q, want the first item", sel.Label)
	}
}

func TestTypingFiltersFuzzily(t *testing.T) {
	m := newModel(kindItems())

	m.push('p')
	m.push('o')
	m.push('d')

	got := visibleKindLabels(m)
	if len(got) == 0 {
		t.Fatal("query \"pod\" matched nothing")
	}
	if got[0] != "pod" {
		t.Errorf("best match = %q, want pod (got %v)", got[0], got)
	}
	for _, label := range got {
		if !strings.Contains(label, "p") {
			t.Errorf("%q does not plausibly match \"pod\"", label)
		}
	}
}

func TestBackspaceRestoresEarlierMatches(t *testing.T) {
	m := newModel(kindItems())
	for _, r := range "pod" {
		m.push(r)
	}
	narrowed := len(m.visible())

	m.backspace()
	m.backspace()
	m.backspace()

	if m.query != "" {
		t.Errorf("query = %q after backspacing it away, want empty", m.query)
	}
	if got := len(m.visible()); got != 4 {
		t.Errorf("visible() = %d after clearing the query (was %d narrowed), want all 4", got, narrowed)
	}
}

func TestBackspaceOnEmptyQueryIsHarmless(t *testing.T) {
	m := newModel(kindItems())
	m.backspace()
	if m.query != "" || len(m.visible()) != 4 {
		t.Errorf("backspace on an empty query changed state: query=%q visible=%d", m.query, len(m.visible()))
	}
}

func TestClearResetsTheQuery(t *testing.T) {
	m := newModel(kindItems())
	for _, r := range "vuln" {
		m.push(r)
	}
	m.clear()

	if m.query != "" {
		t.Errorf("query = %q after clear(), want empty", m.query)
	}
	if len(m.visible()) != 4 {
		t.Errorf("visible() = %d after clear(), want all 4", len(m.visible()))
	}
}

func TestCursorMovesAndClampsAtBothEnds(t *testing.T) {
	m := newModel(kindItems())

	m.up() // already at the top
	if m.cursor != 0 {
		t.Errorf("cursor = %d after up() at the top, want 0", m.cursor)
	}

	for range 10 {
		m.down()
	}
	if want := len(m.visible()) - 1; m.cursor != want {
		t.Errorf("cursor = %d after running past the bottom, want %d", m.cursor, want)
	}

	m.up()
	if want := len(m.visible()) - 2; m.cursor != want {
		t.Errorf("cursor = %d after up(), want %d", m.cursor, want)
	}
}

// A filter that shortens the list must not leave the cursor pointing past the
// end — that would select the wrong item or none at all.
func TestFilteringClampsTheCursor(t *testing.T) {
	m := newModel(kindItems())
	for range 3 {
		m.down()
	}
	if m.cursor != 3 {
		t.Fatalf("cursor = %d, want 3 before filtering", m.cursor)
	}

	for _, r := range "pod" {
		m.push(r)
	}

	if m.cursor >= len(m.visible()) {
		t.Errorf("cursor = %d with only %d visible rows", m.cursor, len(m.visible()))
	}
	sel, ok := m.selected()
	if !ok {
		t.Fatal("selected() = not ok after filtering")
	}
	if sel.Label != m.visible()[m.cursor].Label {
		t.Errorf("selected() = %q, but cursor points at %q", sel.Label, m.visible()[m.cursor].Label)
	}
}

func TestSelectedIsNotOkWhenNothingMatches(t *testing.T) {
	m := newModel(kindItems())
	for _, r := range "zzzznotakind" {
		m.push(r)
	}

	if len(m.visible()) != 0 {
		t.Fatalf("visible() = %v, want no matches", visibleKindLabels(m))
	}
	if _, ok := m.selected(); ok {
		t.Error("selected() = ok with no matches, want not ok")
	}
}

func TestPagingMovesByViewport(t *testing.T) {
	many := make([]Item, 30)
	for i := range many {
		many[i] = Item{Label: string(rune('a'+i%26)) + "-item"}
	}
	m := newModel(many)
	m.viewport = 10

	m.pageDown()
	if m.cursor != 10 {
		t.Errorf("cursor = %d after pageDown with a viewport of 10, want 10", m.cursor)
	}
	m.pageUp()
	if m.cursor != 0 {
		t.Errorf("cursor = %d after pageUp, want 0", m.cursor)
	}
	m.pageUp()
	if m.cursor != 0 {
		t.Errorf("cursor = %d after pageUp at the top, want it clamped to 0", m.cursor)
	}
}

// The header must be measured with the data, or the title row drifts out of
// alignment with the rows beneath it.
func TestHeaderAlignsWithRows(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"a", "short"}},
		{Label: "b", Columns: []string{"bbbbbbbbbb", "x"}},
	}).withHeader(Item{Columns: []string{"KIND", "PLURAL"}})

	header := m.headerRow()
	rows := m.rows()

	want := strings.Index(header, "PLURAL")
	if want < 0 {
		t.Fatalf("header %q has no second column", header)
	}
	for _, row := range rows {
		cell := strings.TrimSpace(row[strings.Index(row, " "):])
		if got := strings.Index(row, cell); got != want {
			t.Errorf("row %q starts its second column at %d, header at %d", row, got, want)
		}
	}
}

// A header wider than any data cell must widen the column, not overflow it.
func TestHeaderWiderThanDataStillAligns(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"a", "x"}},
	}).withHeader(Item{Columns: []string{"AVERYLONGHEADER", "PLURAL"}})

	header := m.headerRow()
	row := m.rows()[0]
	if strings.Index(header, "PLURAL") != strings.Index(row, "x") {
		t.Errorf("columns misaligned when the header is the widest cell:\n%q\n%q", header, row)
	}
}

// Rows are padded per column so the picker lines up like the CLI's tables.
func TestRowsAreColumnAligned(t *testing.T) {
	m := newModel([]Item{
		{Label: "a", Columns: []string{"a", "short"}},
		{Label: "b", Columns: []string{"bbbbbbbbbb", "x"}},
	})

	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows() = %d, want 2", len(rows))
	}
	first := strings.Index(rows[0], "short")
	second := strings.Index(rows[1], "x")
	if first != second {
		t.Errorf("second column starts at %d and %d; columns are not aligned:\n%q\n%q",
			first, second, rows[0], rows[1])
	}
}
