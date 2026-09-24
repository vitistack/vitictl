package picker

import (
	"errors"
	"strings"
	"unicode/utf8"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

// selectInput couples a list choice with one text answer in a single popup:
// pick a row, type a value, one Enter for each. The state is headless for the
// same reason model and textInput are — focus routing is behaviour worth
// testing without a TTY.
type selectInput struct {
	m  *model
	in *textInput
	// inputFocused says where keystrokes go. Enter on the list moves focus
	// to the input; Esc in the input moves it back. The chosen row is
	// whatever the cursor is on at submit, so going back to adjust it never
	// discards the typed value.
	inputFocused bool
}

func newSelectInput(items []Item, header []string, validate func(string) error) *selectInput {
	return &selectInput{
		m:  newModel(items).withHeader(Item{Columns: header}),
		in: newTextInput(validate),
	}
}

// selectInputAction is what a key did to the form.
type selectInputAction int

const (
	selectInputHandled selectInputAction = iota
	selectInputSubmit
	selectInputCancel
)

// handleSelectInputKey routes one termui event ID by focus. Arrows reach the
// list from both halves, so the selection can still be adjusted while the
// value is being typed.
func handleSelectInputKey(f *selectInput, id string) selectInputAction {
	if f.inputFocused {
		switch id {
		case "<Up>":
			f.m.up()
			return selectInputHandled
		case "<Down>":
			f.m.down()
			return selectInputHandled
		}
		switch handleInputKey(f.in, id) {
		case inputCancel:
			// Esc steps back to the list, not out of the popup: the popup's
			// own "back" is cancelling from the list half.
			f.inputFocused = false
		case inputSubmit:
			if _, ok := f.m.selected(); !ok {
				// The filter was narrowed to nothing after focus moved.
				f.inputFocused = false
				return selectInputHandled
			}
			return selectInputSubmit
		}
		return selectInputHandled
	}
	switch handleListKey(f.m, id) {
	case keyCancel:
		return selectInputCancel
	case keyConfirm:
		f.inputFocused = true
	}
	return selectInputHandled
}

// selectionLabel names the row under the cursor in the form's field line:
// the first column if the row has one, the label otherwise.
func (f *selectInput) selectionLabel() string {
	sel, ok := f.m.selected()
	if !ok {
		return ""
	}
	if len(sel.Columns) > 0 {
		return sel.Columns[0]
	}
	return sel.Label
}

// SelectPopupWithInput shows a centered popup that asks two things at once: a
// row from a fuzzy-filtered list and one line of text. Beneath the list sit
// two labelled field lines — selectLabel mirrors the row under the cursor,
// inputLabel holds the typed value — and a ❯ marker walks between them, so
// the Enter that moves focus is visible as movement rather than inferred.
// Enter on the list moves to the field, Enter there submits both; Esc walks
// the same path backwards, leaving the popup only from the list half. The
// validator runs on the text at submit and failures are shown inline.
func (s *Session) SelectPopupWithInput(title string, header []string, items []Item,
	selectLabel, inputLabel, placeholder string, validate func(string) error) (Item, string, error) {
	if s.closed {
		return Item{}, "", errors.New("the picker session is closed")
	}
	if len(items) == 0 {
		return Item{}, "", errors.New("nothing to choose from")
	}

	f := newSelectInput(items, header, validate)

	frame := widgets.NewParagraph()
	frame.Title = title
	frame.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	frame.BorderStyle = ui.NewStyle(ui.ColorGreen)

	search := borderlessParagraph()
	search.TextStyle = ui.NewStyle(ui.ColorWhite)

	headerP := borderlessParagraph()
	headerP.TextStyle = ui.NewStyle(ui.ColorCyan, ui.ColorClear, ui.ModifierBold)

	list := widgets.NewList()
	list.TextStyle = ui.NewStyle(ui.ColorWhite)
	list.WrapText = false
	list.Border = false
	list.PaddingTop = -1
	list.PaddingBottom = -1
	list.PaddingLeft = -1
	list.PaddingRight = -1

	separator := borderlessParagraph()
	separator.TextStyle = ui.NewStyle(ui.ColorGreen)

	selectField := borderlessParagraph()
	inputField := borderlessParagraph()
	hintP := borderlessParagraph()

	// The label column of the two field lines, padded to line up.
	labelW := utf8.RuneCountInString(selectLabel)
	if w := utf8.RuneCountInString(inputLabel); w > labelW {
		labelW = w
	}
	fieldLine := func(marker, label, value string) string {
		return " " + marker + " " + label + strings.Repeat(" ", labelW-utf8.RuneCountInString(label)) + ": " + value
	}

	// The desired size is measured once, on the unfiltered model, so the box
	// does not resize while the user types. Generous margins are the point:
	// a form nobody can read saves no space.
	wantW := 0
	for _, cw := range f.m.columnWidths() {
		wantW += cw + 2
	}
	if pw := labelW + utf8.RuneCountInString(placeholder) + 8; pw > wantW {
		wantW = pw
	}
	if wantW < 56 {
		wantW = 56
	}
	wantW += 8
	rows := len(f.m.filtered)
	const maxFormRows = 12
	if rows > maxFormRows {
		rows = maxFormRows
	}
	// frame(2) + filter + blank + header + rows + blank + separator +
	// two fields + blank + hint
	wantH := 2 + 1 + 1 + 1 + rows + 1 + 1 + 2 + 1 + 1

	draw := func() {
		w, h := ui.TerminalDimensions()
		ui.Clear()
		s.renderBackdrop(w, h)

		x0, y0, x1, y1 := popupRect(w, h, wantW, wantH)
		frame.SetRect(x0, y0, x1, y1)
		// The frame is drawn first and fills its whole rect, so the rows no
		// widget claims stay blank — that is where the breathing room between
		// the sections comes from.
		ix0, iy0, ix1, iy1 := x0+1, y0+1, x1-1, y1-1
		search.SetRect(ix0, iy0, ix1, iy0+1)
		headerP.SetRect(ix0, iy0+2, ix1, iy0+3)
		list.SetRect(ix0, iy0+3, ix1, iy1-6)
		separator.SetRect(ix0, iy1-5, ix1, iy1-4)
		selectField.SetRect(ix0, iy1-4, ix1, iy1-3)
		inputField.SetRect(ix0, iy1-3, ix1, iy1-2)
		hintP.SetRect(ix0, iy1-1, ix1, iy1)
		if inner := list.Inner.Dy(); inner > 0 {
			f.m.viewport = inner
		}

		search.Text = " filter: " + f.m.query + "▏"
		headerP.Text = "   " + f.m.headerRow()
		if len(f.m.filtered) == 0 {
			list.Rows = []string{"", "  no match — press Ctrl-U to clear the filter"}
			list.SelectedRow = 0
		} else {
			rows := f.m.rows()
			for i := range rows {
				rows[i] = "  " + rows[i]
			}
			list.Rows = rows
			list.SelectedRow = f.m.cursor
		}
		separator.Text = " " + strings.Repeat("─", ix1-ix0-2)

		if f.inputFocused {
			// The list half hands over visibly: its cursor bar goes grey, the
			// chosen row is restated with a ✓ on its field line, and the ❯
			// moves down to the value being typed.
			list.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorWhite)
			selectField.Text = fieldLine(" ", selectLabel, f.selectionLabel()+" ✓")
			selectField.TextStyle = ui.NewStyle(ui.ColorGreen)
			value := f.in.value + "▏"
			if f.in.value == "" && placeholder != "" {
				value = "▏" + placeholder
			}
			inputField.Text = fieldLine("❯", inputLabel, value)
			inputField.TextStyle = ui.NewStyle(ui.ColorWhite, ui.ColorClear, ui.ModifierBold)
		} else {
			list.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorGreen, ui.ModifierBold)
			selectField.Text = fieldLine("❯", selectLabel, f.selectionLabel())
			selectField.TextStyle = ui.NewStyle(ui.ColorWhite, ui.ColorClear, ui.ModifierBold)
			value := f.in.value
			if value == "" && placeholder != "" {
				value = placeholder
			}
			inputField.Text = fieldLine(" ", inputLabel, value)
			inputField.TextStyle = ui.NewStyle(ui.ColorBlue)
		}
		if f.in.errMsg != "" {
			hintP.Text = " " + f.in.errMsg
			hintP.TextStyle = ui.NewStyle(ui.ColorRed)
		} else if f.inputFocused {
			hintP.Text = " [Enter] accept  [↑/↓] change " + selectLabel + "  [Esc] back to list"
			hintP.TextStyle = ui.NewStyle(ui.ColorWhite)
		} else {
			hintP.Text = " [type] filter  [↑/↓] move  [Enter] choose " + selectLabel + "  [Esc] back"
			hintP.TextStyle = ui.NewStyle(ui.ColorWhite)
		}

		ui.Render(frame, search, headerP, list, separator, selectField, inputField, hintP)
	}

	draw()
	for e := range s.events {
		switch e.Type {
		case ui.ResizeEvent:
			draw()
			continue
		case ui.KeyboardEvent:
		default:
			continue
		}
		switch handleSelectInputKey(f, e.ID) {
		case selectInputCancel:
			return Item{}, "", ErrCancelled
		case selectInputSubmit:
			chosen, _ := f.m.selected()
			return chosen, f.in.trimmed(), nil
		}
		draw()
	}
	return Item{}, "", ErrCancelled
}
