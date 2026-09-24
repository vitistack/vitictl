package picker

import (
	"errors"
	"os"

	"golang.org/x/term"
)

// ErrCancelled is returned when the user dismisses the picker without
// choosing. Callers should treat it as "no selection", not as a failure.
var ErrCancelled = errors.New("selection cancelled")

// Interactive reports whether a picker can be shown. termui takes over the
// terminal, so it is only safe when the session is attached to one; a piped
// or CI invocation must be told to pass its argument explicitly instead.
func Interactive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// Select shows items in a full-screen list with a fuzzy filter and returns the
// chosen one. It returns ErrCancelled if the user presses Esc, q, or Ctrl-C.
//
// The header row is drawn above the list; pass the column titles matching each
// Item's Columns. For a multi-step flow that should not re-take the terminal
// per step, use NewSession and its methods instead.
func Select(title string, header []string, items []Item) (Item, error) {
	s, err := NewSession()
	if err != nil {
		return Item{}, err
	}
	defer s.Close()
	return s.Select(title, header, items)
}

// SelectMulti is Select with marking: Tab toggles the row under the cursor,
// Ctrl-A toggles every row the filter currently shows, and Enter confirms.
func SelectMulti(title string, header []string, items []Item) ([]Item, error) {
	s, err := NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.SelectMulti(title, header, items)
}
