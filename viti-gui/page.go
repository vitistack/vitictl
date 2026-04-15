package main

import ui "github.com/gizak/termui/v3"

// page is one screen rendered on the right-hand side of the app. A page
// owns its own widgets, handles its own keys, and reports a short help
// hint for the status bar.
type page interface {
	// setRect positions the page's widgets inside the given rectangle.
	setRect(x0, y0, x1, y1 int)
	// render draws the page's widgets. Called after the app has already
	// cleared the screen and drawn the menu + status bar.
	render()
	// handleKey processes an event. Returning true means "consumed";
	// false gives the app a chance to treat it as a global key.
	handleKey(e ui.Event) bool
	// help returns a short hint string shown in the status bar.
	help() string
}
