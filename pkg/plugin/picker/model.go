// Package picker presents interactive fuzzy-filtered lists in the terminal.
//
// The state machine lives in model, separate from any terminal I/O, so the
// interesting behaviour — fuzzy filtering, cursor movement, which rows are
// marked, and which item a selection resolves to — is testable without a TTY.
// Select and SelectMulti add the termui rendering on top.
package picker

import (
	"strings"
	"unicode/utf8"

	"github.com/sahilm/fuzzy"
)

// Item is one selectable row.
type Item struct {
	// Label is what the query is fuzzy-matched against. Include everything
	// worth searching on, not only what is displayed.
	Label string
	// Columns are the cell values rendered for this row, padded into
	// alignment with the other rows.
	Columns []string
	// Value is the caller's own payload for the row.
	Value any
	// Children are rows shown indented beneath this one when it is expanded,
	// and hidden when it is collapsed. A row with children renders a ▸ or ▾
	// disclosure marker and responds to Right and Left.
	//
	// Their labels also join this row's search text, so filtering on something
	// only a child mentions surfaces the parent that owns it rather than an
	// orphan with no context.
	Children []Item
	// Info marks a row that is shown but cannot be chosen: context for the row
	// above it rather than an option of its own. Marking skips it, Ctrl-A skips
	// it, and Enter never resolves to it.
	//
	// Expanding a thing to see what it contains is not the same as offering
	// its contents as alternatives to it. A caller showing what a choice drags
	// along with it wants those rows read, not picked.
	Info bool
}

// defaultViewport is the assumed page size before a terminal height is known.
const defaultViewport = 10

// markerWidth is the width of the "[x] " gutter multi-select rows carry, which
// the header row is indented by to stay aligned with them.
const markerWidth = 4

// model holds the picker's state: the full item set, the current query, the
// filtered view, where the cursor sits within it, and — in multi mode — which
// rows are marked.
//
// filtered holds indices into all rather than copies, so a row's identity
// survives refiltering. That is what lets a mark made under one query still be
// a mark under the next one.
// rowMeta is a flattened row's place in the tree: how deep it sits, which row
// owns it, and whether it owns any itself.
type rowMeta struct {
	depth  int
	parent int
	kids   int
	// search is the row's own label plus every descendant label, so a query
	// that only a child mentions still finds the parent.
	search string
}

type model struct {
	all      []Item
	meta     []rowMeta
	expanded map[int]struct{}
	filtered []int
	query    string
	cursor   int
	viewport int
	// multi enables marking. Kept on the model rather than only in the
	// renderer so the "Enter with nothing marked takes the cursor row" rule
	// is testable.
	multi bool
	// marked holds the indices into all that the user has toggled on.
	marked map[int]struct{}
	// header is measured alongside the data when sizing columns, so the
	// title row lines up with the rows beneath it.
	header Item
}

func newModel(items []Item) *model {
	m := &model{viewport: defaultViewport, marked: map[int]struct{}{},
		expanded: map[int]struct{}{}}
	m.flatten(items, 0, -1)
	m.applyFilter()
	return m
}

// flatten lays the tree out as one slice, recording each row's depth and owner.
//
// Flattening rather than nesting keeps every existing invariant: filtered still
// holds indices into all, so a mark made under one query survives the next, and
// collapsing a parent is a visibility question rather than a restructuring one.
func (m *model) flatten(items []Item, depth, parent int) {
	for _, it := range items {
		idx := len(m.all)
		m.all = append(m.all, it)
		m.meta = append(m.meta, rowMeta{
			depth: depth, parent: parent, kids: len(it.Children),
			search: searchText(it),
		})
		m.flatten(it.Children, depth+1, idx)
	}
}

// searchText is a row's label plus every descendant's, so filtering on a child
// finds the parent that owns it.
func searchText(it Item) string {
	var b strings.Builder
	b.WriteString(it.Label)
	var walk func(kids []Item)
	walk = func(kids []Item) {
		for _, k := range kids {
			b.WriteString(" ")
			b.WriteString(k.Label)
			walk(k.Children)
		}
	}
	walk(it.Children)
	return b.String()
}

// shown reports whether every ancestor of a row is expanded.
func (m *model) shown(i int) bool {
	for p := m.meta[i].parent; p >= 0; p = m.meta[p].parent {
		if _, open := m.expanded[p]; !open {
			return false
		}
	}
	return true
}

// expand opens the row under the cursor, if it has children to show.
func (m *model) expand() {
	idx, ok := m.cursorIndex()
	if !ok || m.meta[idx].kids == 0 {
		return
	}
	m.expanded[idx] = struct{}{}
	m.applyFilter()
}

// collapse closes the row under the cursor, or steps out to its parent when the
// cursor is already on a leaf — which is what Left means in every tree widget.
func (m *model) collapse() {
	idx, ok := m.cursorIndex()
	if !ok {
		return
	}
	if _, open := m.expanded[idx]; open {
		delete(m.expanded, idx)
		m.applyFilter()
		return
	}
	parent := m.meta[idx].parent
	if parent < 0 {
		return
	}
	delete(m.expanded, parent)
	m.applyFilter()
	m.cursorTo(parent)
}

// cursorTo puts the cursor on a row of all, if it is currently visible.
func (m *model) cursorTo(idx int) {
	for pos, i := range m.filtered {
		if i == idx {
			m.cursor = pos
			return
		}
	}
}

// withHeader returns the model set up to reserve width for a header row.
func (m *model) withHeader(header Item) *model {
	m.header = header
	return m
}

// withMulti turns on marking.
func (m *model) withMulti() *model {
	m.multi = true
	return m
}

// headerRow renders the column titles at the same widths as the data rows.
func (m *model) headerRow() string {
	row := renderRow(m.header, m.columnWidths())
	if m.multi {
		return strings.Repeat(" ", markerWidth) + row
	}
	return row
}

// visible returns the rows currently matching the query.
func (m *model) visible() []Item {
	out := make([]Item, 0, len(m.filtered))
	for _, i := range m.filtered {
		out = append(out, m.all[i])
	}
	return out
}

// selected returns the item under the cursor. It reports false when the query
// matches nothing, so callers never select from an empty list.
func (m *model) selected() (Item, bool) {
	i, ok := m.cursorIndex()
	if !ok {
		return Item{}, false
	}
	return m.all[i], true
}

// cursorIndex resolves the cursor to a position in all.
func (m *model) cursorIndex() (int, bool) {
	if m.cursor < 0 || m.cursor >= len(m.filtered) {
		return 0, false
	}
	return m.filtered[m.cursor], true
}

// confirmed returns what Enter resolves to in multi mode: every marked row in
// the original item order, or — when nothing is marked — the row under the
// cursor. Falling back to the cursor row means the common "just this one" case
// needs no marking step at all.
func (m *model) confirmed() []Item {
	if len(m.marked) == 0 {
		if sel, ok := m.selected(); ok && !sel.Info {
			return []Item{sel}
		}
		return nil
	}
	out := make([]Item, 0, len(m.marked))
	for i := range m.all {
		if _, ok := m.marked[i]; ok {
			out = append(out, m.all[i])
		}
	}
	return out
}

// toggle marks or unmarks the row under the cursor and steps down, so holding
// Tab walks a run of rows.
func (m *model) toggle() {
	if !m.multi {
		return
	}
	idx, ok := m.cursorIndex()
	if !ok || m.all[idx].Info {
		// Stepping down anyway keeps Tab usable as a walk through a list that
		// happens to contain context rows.
		m.down()
		return
	}
	if _, on := m.marked[idx]; on {
		delete(m.marked, idx)
	} else {
		m.marked[idx] = struct{}{}
	}
	m.down()
}

// toggleAll marks every currently visible row, or clears them when they are
// already all marked. Filter first, then toggle all: that is how a fleet-wide
// selection is made — type "prod", press Ctrl-A.
func (m *model) toggleAll() {
	if !m.multi || len(m.filtered) == 0 {
		return
	}
	allOn := true
	for _, idx := range m.filtered {
		if m.all[idx].Info {
			continue
		}
		if _, on := m.marked[idx]; !on {
			allOn = false
			break
		}
	}
	for _, idx := range m.filtered {
		if m.all[idx].Info {
			continue
		}
		if allOn {
			delete(m.marked, idx)
			continue
		}
		m.marked[idx] = struct{}{}
	}
}

// hasChildren reports whether any row can expand, so the status line can offer
// the keys only where they do something.
func (m *model) hasChildren() bool {
	for i := range m.meta {
		if m.meta[i].kids > 0 {
			return true
		}
	}
	return false
}

// markedCount reports how many rows are marked, for the status line.
func (m *model) markedCount() int { return len(m.marked) }

// push appends a rune to the query and refilters.
func (m *model) push(r rune) {
	m.query += string(r)
	m.applyFilter()
}

// backspace removes the last rune of the query, if any.
func (m *model) backspace() {
	if n := utf8.RuneCountInString(m.query); n > 0 {
		runes := []rune(m.query)
		m.query = string(runes[:n-1])
		m.applyFilter()
	}
}

// clear empties the query.
func (m *model) clear() {
	m.query = ""
	m.applyFilter()
}

// applyFilter recomputes the visible rows and keeps the cursor in range —
// a shorter list must never leave it pointing past the end.
func (m *model) applyFilter() {
	hit := make([]bool, len(m.all))
	if m.query == "" {
		for i := range hit {
			hit[i] = true
		}
	} else {
		texts := make([]string, len(m.all))
		for i := range m.all {
			texts[i] = m.meta[i].search
		}
		for _, match := range fuzzy.Find(m.query, texts) {
			hit[match.Index] = true
		}
	}

	// A row survives the filter when it matched or an ancestor did: narrowing
	// to a parent means wanting what it contains, not wanting it stripped of
	// its contents. Visibility is then a separate question, so a collapsed
	// parent still hides its children under any query.
	m.filtered = m.filtered[:0]
	for i := range m.all {
		if !m.shown(i) {
			continue
		}
		keep := hit[i]
		for p := m.meta[i].parent; !keep && p >= 0; p = m.meta[p].parent {
			keep = hit[p]
		}
		if keep {
			m.filtered = append(m.filtered, i)
		}
	}
	m.clampCursor()
}

func (m *model) clampCursor() {
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) up()   { m.moveBy(-1) }
func (m *model) down() { m.moveBy(1) }

func (m *model) pageUp()   { m.moveBy(-m.viewport) }
func (m *model) pageDown() { m.moveBy(m.viewport) }

func (m *model) moveBy(delta int) {
	m.cursor += delta
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// displayItem is the row as drawn: its first cell carries the indent and the
// disclosure marker.
//
// Built before measuring rather than glued on after, so the prefix is part of
// the width the other columns align to. Adding it at render time instead pushed
// every expanded row's remaining cells out of the column they shared with their
// neighbours.
func (m *model) displayItem(i int) Item {
	it := m.all[i]
	meta := m.meta[i]
	if meta.depth == 0 && meta.kids == 0 {
		return it
	}
	marker := "  "
	if meta.kids > 0 {
		marker = "▸ "
		if _, open := m.expanded[i]; open {
			marker = "▾ "
		}
	}
	prefix := strings.Repeat("  ", meta.depth) + marker
	cells := make([]string, len(it.Columns))
	copy(cells, it.Columns)
	if len(cells) == 0 {
		cells = []string{prefix}
	} else {
		cells[0] = prefix + cells[0]
	}
	it.Columns = cells
	return it
}

// rows renders the visible items as column-aligned strings, so the picker
// reads like the tables the rest of the CLI prints.
func (m *model) rows() []string {
	widths := m.columnWidths()
	out := make([]string, 0, len(m.filtered))
	for _, idx := range m.filtered {
		row := renderRow(m.displayItem(idx), widths)
		if m.multi {
			// An Info row cannot be marked, so it gets a blank gutter rather
			// than an empty checkbox offering something that is not on offer.
			marker := "    "
			if !m.all[idx].Info {
				marker = "[ ] "
				if _, on := m.marked[idx]; on {
					marker = "[x] "
				}
			}
			row = marker + row
		}
		out = append(out, row)
	}
	return out
}

// columnWidths measures each column across the visible rows and the header,
// so both render at the same widths.
func (m *model) columnWidths() []int {
	var widths []int
	measure := func(it Item) {
		for i, cell := range it.Columns {
			w := len([]rune(cell))
			if i >= len(widths) {
				widths = append(widths, w)
				continue
			}
			if w > widths[i] {
				widths[i] = w
			}
		}
	}
	measure(m.header)
	for _, idx := range m.filtered {
		measure(m.displayItem(idx))
	}
	return widths
}

// renderRow pads an item's cells to the given column widths. The trailing
// column is not padded, so rows carry no trailing whitespace.
func renderRow(it Item, widths []int) string {
	var b strings.Builder
	for i, cell := range it.Columns {
		b.WriteString(cell)
		if i < len(it.Columns)-1 && i < len(widths) {
			b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(cell))+2))
		}
	}
	return strings.TrimRight(b.String(), " ")
}
