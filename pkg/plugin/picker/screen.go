package picker

import (
	"fmt"
	"unicode/utf8"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

// screen is one fuzzy-filtered list: the model plus the widgets that draw it.
// It renders either full-screen (the session's backdrop) or inside a popup
// rect — the layout math is the same, only the framing differs.
type screen struct {
	m     *model
	title string
	// popup draws a single bordered frame around borderless internals, so the
	// list reads as one floating box over the backdrop rather than a stack of
	// bordered widgets.
	popup  bool
	frame  *widgets.Paragraph
	search *widgets.Paragraph
	header *widgets.Paragraph
	list   *widgets.List
	status *widgets.Paragraph
	// wantW/wantH are the popup's desired outer size, measured once at
	// construction (the query is still empty, so every row counts) so the box
	// does not resize while the user filters.
	wantW, wantH int
}

func newScreen(title string, header []string, items []Item, multi, popup bool) *screen {
	sc := &screen{
		m:     newModel(items).withHeader(Item{Columns: header}),
		title: title,
		popup: popup,
	}
	if multi {
		sc.m = sc.m.withMulti()
	}

	sc.list = widgets.NewList()
	sc.list.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorGreen, ui.ModifierBold)
	sc.list.TextStyle = ui.NewStyle(ui.ColorWhite)
	sc.list.WrapText = false

	sc.header = borderlessParagraph()
	sc.header.TextStyle = ui.NewStyle(ui.ColorCyan, ui.ColorClear, ui.ModifierBold)

	sc.status = borderlessParagraph()
	sc.status.TextStyle = ui.NewStyle(ui.ColorWhite)

	if popup {
		sc.frame = widgets.NewParagraph()
		sc.frame.Title = title
		sc.frame.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
		sc.frame.BorderStyle = ui.NewStyle(ui.ColorGreen)

		sc.search = borderlessParagraph()
		sc.search.TextStyle = ui.NewStyle(ui.ColorWhite)

		// A borderless list inside the frame: the same negative-padding trick
		// as borderlessParagraph, because Block.SetRect shrinks Inner by one
		// cell on every side even with Border off.
		sc.list.Border = false
		sc.list.PaddingTop = -1
		sc.list.PaddingBottom = -1
		sc.list.PaddingLeft = -1
		sc.list.PaddingRight = -1

		sc.wantW, sc.wantH = sc.desiredPopupSize()
		return sc
	}

	sc.list.Title = title
	sc.list.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	sc.list.BorderStyle = ui.NewStyle(ui.ColorWhite)

	sc.search = widgets.NewParagraph()
	sc.search.Title = " Filter (fuzzy) "
	sc.search.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	sc.search.BorderStyle = ui.NewStyle(ui.ColorWhite)
	return sc
}

// desiredPopupSize measures the outer box that fits every row and column,
// before any clamping to the terminal.
func (sc *screen) desiredPopupSize() (w, h int) {
	widths := sc.m.columnWidths()
	for _, cw := range widths {
		w += cw
	}
	if len(widths) > 1 {
		w += 2 * (len(widths) - 1) // the column gaps renderRow inserts
	}
	statusW := utf8.RuneCountInString(popupStatusLine(sc.m))
	if statusW > w {
		w = statusW
	}
	w += 4 // frame borders plus one cell of breathing room per side

	rows := len(sc.m.filtered)
	const maxPopupRows = 15
	if rows > maxPopupRows {
		rows = maxPopupRows
	}
	h = 2 + 1 + 1 + rows + 1 // frame + filter + header + rows + status
	return w, h
}

// layout places the widgets inside the given rect and keeps the model's page
// size in step with what actually fits on screen.
func (sc *screen) layout(x0, y0, x1, y1 int) {
	if sc.popup {
		sc.frame.SetRect(x0, y0, x1, y1)
		ix0, iy0, ix1, iy1 := x0+1, y0+1, x1-1, y1-1
		sc.search.SetRect(ix0, iy0, ix1, iy0+1)
		sc.header.SetRect(ix0, iy0+1, ix1, iy0+2)
		sc.list.SetRect(ix0, iy0+2, ix1, iy1-1)
		sc.status.SetRect(ix0, iy1-1, ix1, iy1)
	} else {
		const searchH, headerH, statusH = 3, 1, 1
		sc.search.SetRect(x0, y0, x1, y0+searchH)
		sc.header.SetRect(x0, y0+searchH, x1, y0+searchH+headerH)
		sc.list.SetRect(x0, y0+searchH+headerH, x1, y1-statusH)
		sc.status.SetRect(x0, y1-statusH, x1, y1)
	}
	if inner := sc.list.Inner.Dy(); inner > 0 {
		sc.m.viewport = inner
	}
}

// render refreshes the widget contents from the model and draws them.
func (sc *screen) render() {
	sc.search.Text = " " + sc.m.query + "▏"
	// One leading space to sit under the list's left border.
	sc.header.Text = " " + sc.m.headerRow()
	if sc.popup {
		sc.search.Text = " filter: " + sc.m.query + "▏"
		sc.status.Text = " " + popupStatusLine(sc.m)
	} else {
		sc.status.Text = statusLine(sc.m)
	}

	if len(sc.m.filtered) == 0 {
		sc.list.Rows = []string{"", "  no match — press Ctrl-U to clear the filter"}
		sc.list.SelectedRow = 0
	} else {
		sc.list.Rows = sc.m.rows()
		sc.list.SelectedRow = sc.m.cursor
	}

	if sc.popup {
		ui.Render(sc.frame, sc.search, sc.header, sc.list, sc.status)
		return
	}
	ui.Render(sc.search, sc.header, sc.list, sc.status)
}

// keyAction is what a key did to the list: changed something worth redrawing,
// confirmed a choice, or cancelled.
type keyAction int

const (
	keyHandled keyAction = iota
	keyConfirm
	keyCancel
)

// handleListKey routes one termui event ID to the model. It is the whole key
// map of the picker, kept off the event loop so it is testable without a TTY.
func handleListKey(m *model, id string) keyAction {
	switch id {
	case "<Escape>", "<C-c>", "<C-q>":
		return keyCancel
	case "q":
		// q cancels only when it would not be a filter character.
		if m.query == "" {
			return keyCancel
		}
		m.push('q')
	case "<Enter>":
		if len(m.confirmed()) > 0 {
			return keyConfirm
		}
	case "<Tab>":
		m.toggle()
	case "<C-a>":
		m.toggleAll()
	case "<Right>":
		m.expand()
	case "<Left>":
		m.collapse()
	case "<Up>":
		m.up()
	case "<Down>":
		m.down()
	case "<PageUp>":
		m.pageUp()
	case "<PageDown>":
		m.pageDown()
	case "<Backspace>", "<C-8>":
		m.backspace()
	case "<C-u>":
		m.clear()
	case "<Space>":
		m.push(' ')
	default:
		if utf8.RuneCountInString(id) == 1 {
			m.push([]rune(id)[0])
		}
	}
	return keyHandled
}

// popupRect centers a box of the desired size, clamped to the terminal with a
// two-cell margin. Below a minimum useful terminal it degrades to the full
// screen — a full-screen "popup" is still correct, just not floating.
func popupRect(termW, termH, wantW, wantH int) (x0, y0, x1, y1 int) {
	if termW < 44 || termH < 12 {
		return 0, 0, termW, termH
	}
	w := clampInt(wantW, 40, termW-4)
	h := clampInt(wantH, 8, termH-4)
	x0 = (termW - w) / 2
	y0 = (termH - h) / 2
	return x0, y0, x0 + w, y0 + h
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// statusLine renders the key hints, plus the running mark count in multi mode
// so a selection made under one filter is still visible under the next.
func statusLine(m *model) string {
	// The expand hint appears only when something in the list can expand.
	// Advertising a key that does nothing is how a status line stops being
	// read at all.
	expand := ""
	if m.hasChildren() {
		expand = "  [←/→] collapse/expand"
	}
	if !m.multi {
		return "[type] filter  [↑/↓] move" + expand +
			"  [PgUp/PgDn] page  [Enter] select  [Ctrl-U] clear  [Esc/q] cancel"
	}
	return fmt.Sprintf(
		"[type] filter  [↑/↓] move%s  [Tab] mark  [Ctrl-A] mark all shown  [Enter] confirm (%d marked)  [Ctrl-U] clear  [Esc] cancel",
		expand, m.markedCount())
}

// popupStatusLine is the shorter hint row a popup has room for. Esc reads
// "back" here: it closes the popup, not the command.
func popupStatusLine(m *model) string {
	expand := ""
	if m.hasChildren() {
		expand = "  [←/→] collapse/expand"
	}
	return "[type] filter  [↑/↓] move" + expand + "  [Enter] select  [Esc] back"
}

// borderlessParagraph returns a paragraph that actually fills a one-row rect.
// termui's Block.SetRect shrinks Inner by one cell on every side even when
// Border is false, which would leave nothing drawable; negative padding
// cancels that out.
func borderlessParagraph() *widgets.Paragraph {
	p := widgets.NewParagraph()
	p.Border = false
	p.PaddingTop = -1
	p.PaddingBottom = -1
	p.PaddingLeft = -1
	p.PaddingRight = -1
	return p
}
