package picker

import (
	"errors"
	"testing"
)

func TestTextInputEditing(t *testing.T) {
	in := newTextInput(nil)
	for _, r := range "ab" {
		in.push(r)
	}
	in.push('æ')
	if in.value != "abæ" {
		t.Fatalf("value = %q, want abæ", in.value)
	}
	in.backspace()
	if in.value != "ab" {
		t.Errorf("backspace must remove one rune, not one byte: value = %q", in.value)
	}
	in.clear()
	if in.value != "" {
		t.Errorf("clear must empty the value, got %q", in.value)
	}
	in.backspace() // on empty: harmless
	if in.value != "" {
		t.Errorf("backspace on empty must be a no-op, got %q", in.value)
	}
}

func TestHandleInputKeyTreatsQAsALiteral(t *testing.T) {
	in := newTextInput(nil)
	if got := handleInputKey(in, "q"); got != inputHandled {
		t.Fatalf("q must be typed into an input, not cancel it (got action %d)", got)
	}
	if in.value != "q" {
		t.Errorf("value = %q, want q", in.value)
	}
}

func TestHandleInputKeyEditingKeys(t *testing.T) {
	in := newTextInput(nil)
	handleInputKey(in, "a")
	handleInputKey(in, "<Space>")
	handleInputKey(in, "b")
	if in.value != "a b" {
		t.Fatalf("value = %q, want %q", in.value, "a b")
	}
	handleInputKey(in, "<Backspace>")
	if in.value != "a " {
		t.Errorf("value after backspace = %q, want %q", in.value, "a ")
	}
	handleInputKey(in, "<C-u>")
	if in.value != "" {
		t.Errorf("Ctrl-U must clear the value, got %q", in.value)
	}
}

func TestHandleInputKeyCancels(t *testing.T) {
	for _, id := range []string{"<Escape>", "<C-c>", "<C-q>"} {
		in := newTextInput(nil)
		if got := handleInputKey(in, id); got != inputCancel {
			t.Errorf("%s must cancel the input, got action %d", id, got)
		}
	}
}

func TestHandleInputKeyValidatesOnEnter(t *testing.T) {
	in := newTextInput(func(v string) error {
		if v != "3" {
			return errors.New("want exactly 3")
		}
		return nil
	})

	handleInputKey(in, "4")
	if got := handleInputKey(in, "<Enter>"); got != inputHandled {
		t.Fatalf("Enter with a failing validator must keep the popup open, got action %d", got)
	}
	if in.errMsg != "want exactly 3" {
		t.Errorf("errMsg = %q, want the validator's message", in.errMsg)
	}

	// Any edit clears the error: it described a value that no longer exists.
	handleInputKey(in, "<Backspace>")
	if in.errMsg != "" {
		t.Errorf("an edit must clear errMsg, got %q", in.errMsg)
	}

	handleInputKey(in, "3")
	if got := handleInputKey(in, "<Enter>"); got != inputSubmit {
		t.Errorf("Enter with a passing validator must submit, got action %d", got)
	}
}

func TestInputSubmitsTheTrimmedValue(t *testing.T) {
	var seen string
	in := newTextInput(func(v string) error {
		seen = v
		return nil
	})
	for _, r := range "  3 " {
		in.push(r)
	}
	if got := handleInputKey(in, "<Enter>"); got != inputSubmit {
		t.Fatalf("expected submit, got action %d", got)
	}
	if seen != "3" || in.trimmed() != "3" {
		t.Errorf("validator saw %q, trimmed() = %q — both must be the trimmed value", seen, in.trimmed())
	}
}
