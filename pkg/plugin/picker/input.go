package picker

import (
	"strings"
	"unicode/utf8"
)

// textInput is the state of a one-line input popup, separate from any
// terminal I/O — the same split as model, and for the same reason: editing
// and validation are testable without a TTY.
type textInput struct {
	value string
	// errMsg is the last validation failure, shown inline in the popup. Any
	// edit clears it: the message described a value that no longer exists.
	errMsg   string
	validate func(string) error
}

func newTextInput(validate func(string) error) *textInput {
	return &textInput{validate: validate}
}

func (t *textInput) push(r rune) {
	t.value += string(r)
	t.errMsg = ""
}

func (t *textInput) backspace() {
	if n := utf8.RuneCountInString(t.value); n > 0 {
		runes := []rune(t.value)
		t.value = string(runes[:n-1])
		t.errMsg = ""
	}
}

func (t *textInput) clear() {
	t.value = ""
	t.errMsg = ""
}

// trimmed is what submit returns and what the validator judges: surrounding
// whitespace is never part of an answer.
func (t *textInput) trimmed() string {
	return strings.TrimSpace(t.value)
}

// inputAction is what a key did to the input: changed something worth
// redrawing, submitted a valid value, or cancelled.
type inputAction int

const (
	inputHandled inputAction = iota
	inputSubmit
	inputCancel
)

// handleInputKey routes one termui event ID. On Enter the validator runs; a
// failure is recorded as errMsg and the popup stays open. Unlike the list, a
// bare q is always a literal character here — an input field that cannot
// contain a q is a trap.
func handleInputKey(t *textInput, id string) inputAction {
	switch id {
	case "<Escape>", "<C-c>", "<C-q>":
		return inputCancel
	case "<Enter>":
		if t.validate != nil {
			if err := t.validate(t.trimmed()); err != nil {
				t.errMsg = err.Error()
				return inputHandled
			}
		}
		return inputSubmit
	case "<Backspace>", "<C-8>":
		t.backspace()
	case "<C-u>":
		t.clear()
	case "<Space>":
		t.push(' ')
	default:
		if utf8.RuneCountInString(id) == 1 {
			t.push([]rune(id)[0])
		}
	}
	return inputHandled
}
