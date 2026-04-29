package main

import (
	"fmt"

	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
)

// app owns the top-level TUI state: the left menu, the currently active
// page on the right, and the global event loop.
type app struct {
	menu       *widgets.List
	items      []menuItem
	page       page
	status     *widgets.Paragraph
	width      int
	height     int
	focused    focus
	shouldQuit bool
}

type focus int

const (
	focusMenu focus = iota
	focusPage
)

type menuItem struct {
	label string
	build func(*app) page // factory for the page when the item is activated
}

func run() error {
	if err := ui.Init(); err != nil {
		return fmt.Errorf("init termui: %w", err)
	}
	defer ui.Close()

	a := newApp()
	a.layout()
	a.render()

	for e := range ui.PollEvents() {
		if e.Type == ui.ResizeEvent {
			a.width, a.height = ui.TerminalDimensions()
			a.layout()
			a.render()
			continue
		}
		if !a.handleGlobalKey(e) {
			// Not consumed globally — forward to the focused pane.
			consumed := false
			if a.focused == focusPage && a.page != nil {
				consumed = a.page.handleKey(e)
			} else {
				consumed = a.handleMenuKey(e)
			}
			if consumed {
				a.render()
				continue
			}
		}
		if a.shouldQuit {
			return nil
		}
		a.render()
	}
	return nil
}

func newApp() *app {
	w, h := ui.TerminalDimensions()
	a := &app{
		width:   w,
		height:  h,
		focused: focusMenu,
	}

	a.items = []menuItem{
		{label: "Secrets", build: func(aa *app) page { return newSecretsPage(aa) }},
	}

	list := widgets.NewList()
	list.Title = " Menu "
	list.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	list.BorderStyle = ui.NewStyle(ui.ColorGreen)
	list.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorGreen, ui.ModifierBold)
	list.TextStyle = ui.NewStyle(ui.ColorWhite)
	list.WrapText = false
	rows := make([]string, len(a.items))
	for i, it := range a.items {
		rows[i] = " " + it.label
	}
	list.Rows = rows
	a.menu = list

	status := widgets.NewParagraph()
	status.Border = false
	status.TextStyle = ui.NewStyle(ui.ColorWhite)
	// termui's Block.SetRect unconditionally shrinks Inner by one cell on
	// every side, even when Border is false — a 2-row outer rect ends up
	// with a 0-row inner rect and the help text never gets drawn. Negative
	// padding cancels that out so Inner covers the full status area.
	status.PaddingTop = -1
	status.PaddingBottom = -1
	status.PaddingLeft = -1
	status.PaddingRight = -1
	a.status = status

	a.setStatus()
	return a
}

func (a *app) setStatus() {
	var help string
	if a.page == nil {
		help = "[↑/↓] select menu  [Enter] open  [Ctrl-Q / q] quit"
	} else {
		// Page help may already be multi-line; put the global suffix on
		// its own line so it never gets pushed off-screen by a long
		// page-specific binding row.
		help = a.page.help() + "\n[Esc] back  [Ctrl-Q / q] quit"
	}
	a.status.Text = help
}

func (a *app) layout() {
	// Layout constants.
	menuWidth := 22
	if a.width < 60 {
		menuWidth = a.width / 3
	}
	// Three rows so the secrets detail view's two help lines plus the
	// global "[Esc] back / [Ctrl-Q] quit" suffix line are all visible
	// even on narrow terminals where each line would otherwise wrap.
	statusHeight := 3

	a.menu.SetRect(0, 0, menuWidth, a.height-statusHeight)
	if a.page != nil {
		a.page.setRect(menuWidth, 0, a.width, a.height-statusHeight)
	}
	a.status.SetRect(0, a.height-statusHeight, a.width, a.height)
	a.setStatus()
}

func (a *app) render() {
	ui.Clear()
	ui.Render(a.menu)
	if a.page != nil {
		a.page.render()
	} else {
		// Render a welcome placeholder inside the content area.
		p := widgets.NewParagraph()
		p.Title = " viti-gui "
		p.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
		p.BorderStyle = ui.NewStyle(ui.ColorWhite)
		p.Text = "\n  Pick a menu entry on the left. Press Enter to open it,\n  Esc to come back, q to quit."
		menuRight := a.menu.Inner.Max.X + 1
		p.SetRect(menuRight, 0, a.width, a.height-1)
		ui.Render(p)
	}
	ui.Render(a.status)
}

func (a *app) handleGlobalKey(e ui.Event) bool {
	switch e.ID {
	case "q", "<C-q>":
		a.shouldQuit = true
		return true
	case "<Tab>":
		if a.focused == focusMenu {
			a.focused = focusPage
		} else {
			a.focused = focusMenu
		}
		return true
	}
	return false
}

func (a *app) handleMenuKey(e ui.Event) bool {
	switch e.ID {
	case "<Up>", "k":
		a.menu.ScrollUp()
	case "<Down>", "j":
		a.menu.ScrollDown()
	case "<Enter>":
		if a.menu.SelectedRow < 0 || a.menu.SelectedRow >= len(a.items) {
			return true
		}
		a.openPage(a.items[a.menu.SelectedRow])
	default:
		return false
	}
	return true
}

func (a *app) openPage(item menuItem) {
	a.page = item.build(a)
	a.focused = focusPage
	a.layout()
	a.setStatus()
}

func (a *app) closePage() {
	a.page = nil
	a.focused = focusMenu
	a.setStatus()
}
