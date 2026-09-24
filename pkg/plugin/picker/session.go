package picker

import (
	"errors"
	"fmt"
	"unicode/utf8"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

// Session owns one termui lifetime, so a multi-step flow renders as one
// continuous screen instead of a full terminal takeover per question. The
// last full-screen Select stays visible as the backdrop, and later choices
// float over it as centered popups.
type Session struct {
	// events is taken exactly once: every PollEvents call spawns its own
	// termbox poller, and two pollers split the keystrokes between them.
	events <-chan ui.Event
	// base is the last full-screen Select, redrawn under every popup.
	base *screen
	// context is the one-line backdrop drawn when no base screen exists —
	// a flag-driven run that only needs one popup still shows where it is.
	context string
	closed  bool
}

// NewSession takes over the terminal. Close must be called before anything
// else writes to stdout or reads stdin.
func NewSession() (*Session, error) {
	if err := ui.Init(); err != nil {
		return nil, fmt.Errorf("starting the terminal UI: %w", err)
	}
	return &Session{events: ui.PollEvents()}, nil
}

// Close restores the terminal. It is idempotent, so a deferred Close and an
// explicit early one (before printing results) can coexist.
func (s *Session) Close() {
	if s.closed {
		return
	}
	s.closed = true
	ui.Close()
}

// SetContext sets the backdrop line shown behind popups opened before any
// full-screen Select. A base screen, once shown, takes precedence.
func (s *Session) SetContext(line string) {
	s.context = line
}

// Select shows items full-screen and returns the chosen one. The screen stays
// on the terminal afterwards, as the backdrop for any popups that follow.
func (s *Session) Select(title string, header []string, items []Item) (Item, error) {
	chosen, err := s.pickList(title, header, items, false, false)
	if err != nil {
		return Item{}, err
	}
	return chosen[0], nil
}

// SelectMulti is Select with marking: Tab toggles the row under the cursor,
// Ctrl-A toggles every row the filter currently shows, and Enter confirms.
//
// Enter with nothing marked returns the row under the cursor, so choosing one
// item costs no more keystrokes than in the single-select picker. Filtering
// then Ctrl-A is the fleet path: type "prod", mark all sixteen, Enter.
func (s *Session) SelectMulti(title string, header []string, items []Item) ([]Item, error) {
	return s.pickList(title, header, items, true, false)
}

// SelectPopup is Select drawn as a centered overlay with the fuzzy filter
// intact; the backdrop stays visible around it. Esc cancels only this popup —
// ErrCancelled comes back and the session stays usable, which is what lets a
// caller treat it as "go back one step".
func (s *Session) SelectPopup(title string, header []string, items []Item) (Item, error) {
	chosen, err := s.pickList(title, header, items, false, true)
	if err != nil {
		return Item{}, err
	}
	return chosen[0], nil
}

// pickList is the shared list loop for full-screen and popup picks.
func (s *Session) pickList(title string, header []string, items []Item, multi, popup bool) ([]Item, error) {
	if s.closed {
		return nil, errors.New("the picker session is closed")
	}
	if len(items) == 0 {
		return nil, errors.New("nothing to choose from")
	}

	sc := newScreen(title, header, items, multi, popup)
	if !popup {
		s.base = sc
	}
	draw := func() {
		w, h := ui.TerminalDimensions()
		ui.Clear()
		if popup {
			s.renderBackdrop(w, h)
			sc.layout(popupRect(w, h, sc.wantW, sc.wantH))
		} else {
			sc.layout(0, 0, w, h)
		}
		sc.render()
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
		switch handleListKey(sc.m, e.ID) {
		case keyCancel:
			return nil, ErrCancelled
		case keyConfirm:
			return sc.m.confirmed(), nil
		}
		draw()
	}
	return nil, ErrCancelled
}

// Input asks for one line of text in a centered popup. The validator runs on
// Enter; a failure is shown inline in red and the popup stays open. It
// returns the trimmed value, or ErrCancelled when the user backs out.
func (s *Session) Input(title, prompt, placeholder string, validate func(string) error) (string, error) {
	if s.closed {
		return "", errors.New("the picker session is closed")
	}

	t := newTextInput(validate)

	frame := widgets.NewParagraph()
	frame.Title = title
	frame.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	frame.BorderStyle = ui.NewStyle(ui.ColorGreen)

	promptP := borderlessParagraph()
	promptP.TextStyle = ui.NewStyle(ui.ColorWhite)
	promptP.Text = " " + prompt

	inputP := borderlessParagraph()
	hintP := borderlessParagraph()

	wantW := utf8.RuneCountInString(prompt)
	if pw := utf8.RuneCountInString(placeholder); pw > wantW {
		wantW = pw
	}
	if wantW < 46 {
		wantW = 46
	}
	wantW += 4
	const wantH = 2 + 3 // frame + prompt + input + hint/error

	draw := func() {
		w, h := ui.TerminalDimensions()
		ui.Clear()
		s.renderBackdrop(w, h)

		x0, y0, x1, y1 := popupRect(w, h, wantW, wantH)
		frame.SetRect(x0, y0, x1, y1)
		ix0, iy0, ix1 := x0+1, y0+1, x1-1
		promptP.SetRect(ix0, iy0, ix1, iy0+1)
		inputP.SetRect(ix0, iy0+1, ix1, iy0+2)
		hintP.SetRect(ix0, iy0+2, ix1, iy0+3)

		if t.value == "" && placeholder != "" {
			inputP.Text = " ▏" + placeholder
			inputP.TextStyle = ui.NewStyle(ui.ColorBlue)
		} else {
			inputP.Text = " " + t.value + "▏"
			inputP.TextStyle = ui.NewStyle(ui.ColorWhite, ui.ColorClear, ui.ModifierBold)
		}
		if t.errMsg != "" {
			hintP.Text = " " + t.errMsg
			hintP.TextStyle = ui.NewStyle(ui.ColorRed)
		} else {
			hintP.Text = " [Enter] accept  [Esc] back  [Ctrl-U] clear"
			hintP.TextStyle = ui.NewStyle(ui.ColorWhite)
		}

		ui.Render(frame, promptP, inputP, hintP)
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
		switch handleInputKey(t, e.ID) {
		case inputCancel:
			return "", ErrCancelled
		case inputSubmit:
			return t.trimmed(), nil
		}
		draw()
	}
	return "", ErrCancelled
}

// renderBackdrop draws what sits behind a popup: the last full-screen pick
// with its cursor still on the row the user chose, or the context line.
func (s *Session) renderBackdrop(w, h int) {
	if s.base != nil {
		s.base.layout(0, 0, w, h)
		s.base.render()
		return
	}
	if s.context == "" {
		return
	}
	p := borderlessParagraph()
	p.Text = " " + s.context
	p.TextStyle = ui.NewStyle(ui.ColorCyan, ui.ColorClear, ui.ModifierBold)
	p.SetRect(0, 0, w, 1)
	ui.Render(p)
}
