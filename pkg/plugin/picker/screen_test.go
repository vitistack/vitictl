package picker

import (
	"strings"
	"testing"
)

func TestPopupRectCentersOnARoomyTerminal(t *testing.T) {
	x0, y0, x1, y1 := popupRect(120, 40, 60, 20)
	if x1-x0 != 60 || y1-y0 != 20 {
		t.Fatalf("got %dx%d, want the desired 60x20", x1-x0, y1-y0)
	}
	if x0 != 30 || y0 != 10 {
		t.Errorf("got origin (%d,%d), want centered (30,10)", x0, y0)
	}
}

func TestPopupRectClampsToTheTerminalWithAMargin(t *testing.T) {
	x0, y0, x1, y1 := popupRect(80, 24, 200, 100)
	if x1-x0 != 76 || y1-y0 != 20 {
		t.Errorf("got %dx%d, want clamped to 76x20 (terminal minus the margin)", x1-x0, y1-y0)
	}
	if x0 < 0 || y0 < 0 || x1 > 80 || y1 > 24 {
		t.Errorf("rect (%d,%d)-(%d,%d) leaves the terminal", x0, y0, x1, y1)
	}
}

func TestPopupRectEnforcesAMinimumSize(t *testing.T) {
	x0, y0, x1, y1 := popupRect(120, 40, 10, 3)
	if x1-x0 != 40 || y1-y0 != 8 {
		t.Errorf("got %dx%d, want the 40x8 minimum", x1-x0, y1-y0)
	}
	_ = x0
	_ = y0
}

func TestPopupRectDegradesToFullScreenOnATinyTerminal(t *testing.T) {
	for _, tc := range [][2]int{{30, 8}, {43, 40}, {120, 11}} {
		x0, y0, x1, y1 := popupRect(tc[0], tc[1], 60, 20)
		if x0 != 0 || y0 != 0 || x1 != tc[0] || y1 != tc[1] {
			t.Errorf("on a %dx%d terminal the popup must fill it, got (%d,%d)-(%d,%d)",
				tc[0], tc[1], x0, y0, x1, y1)
		}
	}
}

func TestHandleListKeyCancels(t *testing.T) {
	for _, id := range []string{"<Escape>", "<C-c>", "<C-q>"} {
		m := newModel(items("alpha", "beta"))
		if got := handleListKey(m, id); got != keyCancel {
			t.Errorf("%s must cancel, got action %d", id, got)
		}
	}
}

func TestHandleListKeyQCancelsOnlyOnAnEmptyQuery(t *testing.T) {
	m := newModel(items("alpha", "beta"))
	if got := handleListKey(m, "q"); got != keyCancel {
		t.Fatalf("q on an empty query must cancel, got action %d", got)
	}

	m = newModel(items("quick", "beta"))
	handleListKey(m, "u")
	if got := handleListKey(m, "q"); got == keyCancel {
		t.Fatal("q with a query in progress is a filter character, not a cancel")
	}
	if m.query != "uq" {
		t.Errorf("query = %q, want uq", m.query)
	}
}

func TestHandleListKeyConfirmsOnlyWhenSomethingMatches(t *testing.T) {
	m := newModel(items("alpha", "beta"))
	if got := handleListKey(m, "<Enter>"); got != keyConfirm {
		t.Fatalf("Enter on a visible row must confirm, got action %d", got)
	}

	for _, r := range "zzz" {
		handleListKey(m, string(r))
	}
	if got := handleListKey(m, "<Enter>"); got == keyConfirm {
		t.Error("Enter with nothing matching must not confirm")
	}
}

func TestHandleListKeyRoutesMovementAndEditing(t *testing.T) {
	m := newModel(items("alpha", "beta", "gamma"))
	handleListKey(m, "<Down>")
	handleListKey(m, "<Down>")
	if m.cursor != 2 {
		t.Fatalf("cursor = %d after two Downs, want 2", m.cursor)
	}
	handleListKey(m, "<Up>")
	if m.cursor != 1 {
		t.Fatalf("cursor = %d after Up, want 1", m.cursor)
	}
	handleListKey(m, "b")
	handleListKey(m, "<Backspace>")
	handleListKey(m, "<C-u>")
	if m.query != "" {
		t.Errorf("query = %q after backspace and clear, want empty", m.query)
	}
}

func TestPopupStatusLineSaysBack(t *testing.T) {
	m := newModel(items("alpha"))
	got := popupStatusLine(m)
	if want := "[Esc] back"; !strings.Contains(got, want) {
		t.Errorf("popup status %q must offer %q — Esc closes the popup, not the command", got, want)
	}
}
