package picker

import (
	"errors"
	"testing"
)

func newTestForm(validate func(string) error, labels ...string) *selectInput {
	return newSelectInput(items(labels...), []string{"NAME"}, validate)
}

func TestFormEnterOnTheListMovesFocusToTheInput(t *testing.T) {
	f := newTestForm(nil, "alpha", "beta")
	if got := handleSelectInputKey(f, "<Enter>"); got != selectInputHandled {
		t.Fatalf("the first Enter moves focus, it must not submit (got action %d)", got)
	}
	if !f.inputFocused {
		t.Fatal("Enter on the list must focus the input")
	}
}

func TestFormRoutesTypingByFocus(t *testing.T) {
	f := newTestForm(nil, "alpha", "beta")
	handleSelectInputKey(f, "b")
	if f.m.query != "b" || f.in.value != "" {
		t.Fatalf("with the list focused, typing filters: query=%q value=%q", f.m.query, f.in.value)
	}
	handleSelectInputKey(f, "<C-u>")
	handleSelectInputKey(f, "<Enter>")
	handleSelectInputKey(f, "3")
	if f.in.value != "3" || f.m.query != "" {
		t.Errorf("with the input focused, typing edits the value: query=%q value=%q", f.m.query, f.in.value)
	}
}

func TestFormEscWalksBackwards(t *testing.T) {
	f := newTestForm(nil, "alpha")
	handleSelectInputKey(f, "<Enter>")
	if got := handleSelectInputKey(f, "<Escape>"); got != selectInputHandled || f.inputFocused {
		t.Fatalf("Esc in the input must return to the list, not cancel (action %d, focused %v)", got, f.inputFocused)
	}
	if got := handleSelectInputKey(f, "<Escape>"); got != selectInputCancel {
		t.Errorf("Esc on the list must cancel the popup, got action %d", got)
	}
}

func TestFormArrowsReachTheListFromTheInput(t *testing.T) {
	f := newTestForm(nil, "alpha", "beta", "gamma")
	handleSelectInputKey(f, "<Enter>")
	handleSelectInputKey(f, "<Down>")
	handleSelectInputKey(f, "<Down>")
	if f.m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2 — the selection must stay adjustable while typing", f.m.cursor)
	}
	handleSelectInputKey(f, "<Up>")
	sel, _ := f.m.selected()
	if sel.Label != "beta" {
		t.Errorf("selected %q, want beta", sel.Label)
	}
}

func TestFormSubmitValidatesAndReturnsBoth(t *testing.T) {
	f := newTestForm(func(v string) error {
		if v != "3" {
			return errors.New("want 3")
		}
		return nil
	}, "alpha", "beta")

	handleSelectInputKey(f, "<Down>")
	handleSelectInputKey(f, "<Enter>")
	handleSelectInputKey(f, "4")
	if got := handleSelectInputKey(f, "<Enter>"); got != selectInputHandled || f.in.errMsg == "" {
		t.Fatalf("an invalid value must keep the popup open with the error inline (action %d, err %q)",
			got, f.in.errMsg)
	}
	handleSelectInputKey(f, "<Backspace>")
	handleSelectInputKey(f, "3")
	if got := handleSelectInputKey(f, "<Enter>"); got != selectInputSubmit {
		t.Fatalf("a valid value must submit, got action %d", got)
	}
	sel, ok := f.m.selected()
	if !ok || sel.Label != "beta" || f.in.trimmed() != "3" {
		t.Errorf("submit must carry both answers: selected %q, value %q", sel.Label, f.in.trimmed())
	}
}

func TestFormQIsALiteralInTheInputHalf(t *testing.T) {
	f := newTestForm(nil, "alpha")
	handleSelectInputKey(f, "<Enter>")
	if got := handleSelectInputKey(f, "q"); got != selectInputHandled || f.in.value != "q" {
		t.Errorf("q in the input half is a character, not a cancel (action %d, value %q)", got, f.in.value)
	}
}

func TestFormRefusesToSubmitWithNothingSelected(t *testing.T) {
	f := newTestForm(nil, "alpha")
	handleSelectInputKey(f, "<Enter>") // focus the input
	// Narrow the list to nothing behind the input's back.
	for _, r := range "zzz" {
		f.m.push(r)
	}
	if got := handleSelectInputKey(f, "<Enter>"); got == selectInputSubmit {
		t.Fatal("submit with no selected row must not succeed")
	}
	if f.inputFocused {
		t.Error("a submit with nothing selected must hand focus back to the list")
	}
}
