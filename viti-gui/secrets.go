package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/atotto/clipboard"
	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
	corev1 "k8s.io/api/core/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vitiv1alpha1 "github.com/vitistack/common/pkg/v1alpha1"
	"github.com/vitistack/vitictl/internal/extract"
	"github.com/vitistack/vitictl/internal/fuzzy"
	"github.com/vitistack/vitictl/internal/kube"
	"github.com/vitistack/vitictl/internal/settings"
)

// secretsPage implements the "Secrets" menu entry. It has two internal
// states: a cluster picker (list + fuzzy search) and a secret viewer
// (keys list + value pane). Esc from the viewer returns to the picker.
type secretsPage struct {
	app *app

	// cluster-list state
	clusters  []clusterEntry
	query     string
	filtered  []clusterEntry
	selected  int // index into filtered
	listView  *widgets.List
	search    *widgets.Paragraph
	errBanner *widgets.Paragraph

	// secret-detail state
	activeCluster *clusterEntry
	secret        *corev1.Secret
	keys          []string
	keyIdx        int
	decodeB64     bool
	showAll       bool
	keysView      *widgets.List
	valueView     *widgets.List

	// layout
	rect image
}

type image struct{ x0, y0, x1, y1 int }

type clusterEntry struct {
	azName    string
	client    *kube.Client
	cluster   *vitiv1alpha1.KubernetesCluster
	label     string // "<az>/<namespace>/<name>"
	clusterID string
}

func newSecretsPage(a *app) page {
	p := &secretsPage{app: a}
	p.initWidgets()
	p.loadClusters()
	return p
}

func (p *secretsPage) initWidgets() {
	p.listView = widgets.NewList()
	p.listView.Title = " KubernetesClusters "
	p.listView.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	p.listView.BorderStyle = ui.NewStyle(ui.ColorWhite)
	p.listView.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorYellow, ui.ModifierBold)
	p.listView.WrapText = false

	p.search = widgets.NewParagraph()
	p.search.Title = " Search (fuzzy) "
	p.search.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	p.search.BorderStyle = ui.NewStyle(ui.ColorWhite)

	p.errBanner = widgets.NewParagraph()
	p.errBanner.Border = false
	p.errBanner.TextStyle = ui.NewStyle(ui.ColorRed)

	p.keysView = widgets.NewList()
	p.keysView.Title = " Keys "
	p.keysView.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	p.keysView.BorderStyle = ui.NewStyle(ui.ColorWhite)
	p.keysView.SelectedRowStyle = ui.NewStyle(ui.ColorBlack, ui.ColorYellow, ui.ModifierBold)
	p.keysView.WrapText = false

	p.valueView = widgets.NewList()
	p.valueView.Title = " Value "
	p.valueView.TitleStyle = ui.NewStyle(ui.ColorGreen, ui.ColorClear, ui.ModifierBold)
	p.valueView.BorderStyle = ui.NewStyle(ui.ColorWhite)
	p.valueView.WrapText = true
	// SelectedRow is repurposed as the scroll cursor — match the
	// unselected text style so it doesn't render as a highlighted row.
	p.valueView.TextStyle = ui.NewStyle(ui.ColorWhite)
	p.valueView.SelectedRowStyle = ui.NewStyle(ui.ColorWhite)
}

// loadClusters populates p.clusters by connecting to every configured
// availability zone and listing KubernetesCluster resources. Unreachable
// zones are surfaced in the error banner rather than aborting.
func (p *secretsPage) loadClusters() {
	p.query = ""
	p.selected = 0

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	zones, err := settings.AvailabilityZones()
	if err != nil {
		p.showError(fmt.Sprintf("reading availability zones: %v", err))
		return
	}
	if len(zones) == 0 {
		p.showError("no availability zones configured — run `viti config init`")
		return
	}

	var warnings []string
	clients, err := kube.ConnectAll(ctx, zones, true, func(werr error) {
		warnings = append(warnings, werr.Error())
	})
	if err != nil {
		p.showError(fmt.Sprintf("connect: %v", err))
		return
	}

	var all []clusterEntry
	for _, c := range clients {
		var list vitiv1alpha1.KubernetesClusterList
		if err := c.Ctrl.List(ctx, &list, &ctrlclient.ListOptions{}); err != nil {
			warnings = append(warnings, fmt.Sprintf("list clusters in %q: %v", c.AZ.Name, err))
			continue
		}
		for i := range list.Items {
			kc := &list.Items[i]
			all = append(all, clusterEntry{
				azName:    c.AZ.Name,
				client:    c,
				cluster:   kc,
				label:     fmt.Sprintf("%s/%s/%s", c.AZ.Name, kc.Namespace, kc.Name),
				clusterID: kc.Spec.Cluster.ClusterId,
			})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].label < all[j].label })
	p.clusters = all
	p.applyFilter()
	if len(warnings) > 0 {
		p.showError("warnings: " + strings.Join(warnings, "; "))
	} else {
		p.errBanner.Text = ""
	}
}

func (p *secretsPage) applyFilter() {
	candidates := make([]fuzzy.Candidate[clusterEntry], len(p.clusters))
	for i, c := range p.clusters {
		candidates[i] = fuzzy.Candidate[clusterEntry]{Label: c.label, Item: c}
	}
	matches := fuzzy.Search(p.query, candidates)
	p.filtered = make([]clusterEntry, len(matches))
	for i, m := range matches {
		p.filtered[i] = m.Item
	}
	if p.selected >= len(p.filtered) {
		p.selected = 0
	}
	rows := make([]string, len(p.filtered))
	for i, c := range p.filtered {
		rows[i] = " " + c.label
	}
	p.listView.Rows = rows
	if len(rows) == 0 {
		p.listView.SelectedRow = 0
	} else {
		if p.selected < 0 {
			p.selected = 0
		}
		p.listView.SelectedRow = p.selected
	}
}

func (p *secretsPage) setRect(x0, y0, x1, y1 int) {
	p.rect = image{x0, y0, x1, y1}
}

func (p *secretsPage) render() {
	if p.activeCluster != nil {
		p.renderDetail()
	} else {
		p.renderPicker()
	}
}

func (p *secretsPage) renderPicker() {
	x0, y0, x1, y1 := p.rect.x0, p.rect.y0, p.rect.x1, p.rect.y1
	searchH := 3
	errH := 0
	if p.errBanner.Text != "" {
		errH = 1
	}

	if errH > 0 {
		p.errBanner.SetRect(x0, y0, x1, y0+errH)
		ui.Render(p.errBanner)
	}
	contentY0 := y0 + errH

	p.search.Text = " " + p.query + "▏"
	p.search.SetRect(x0, contentY0, x1, contentY0+searchH)
	p.listView.SetRect(x0, contentY0+searchH, x1, y1)
	ui.Render(p.search, p.listView)
}

func (p *secretsPage) renderDetail() {
	x0, y0, x1, y1 := p.rect.x0, p.rect.y0, p.rect.x1, p.rect.y1
	errH := 0
	if p.errBanner.Text != "" {
		errH = 1
	}

	if errH > 0 {
		p.errBanner.SetRect(x0, y0, x1, y0+errH)
		ui.Render(p.errBanner)
	}
	contentY0 := y0 + errH

	// Split right region: 30% keys, 70% value.
	split := x0 + (x1-x0)/3
	if split-x0 < 20 {
		split = x0 + 20
	}
	p.keysView.SetRect(x0, contentY0, split, y1)
	p.valueView.SetRect(split, contentY0, x1, y1)

	// Refresh key list rows.
	rows := make([]string, len(p.keys))
	for i, k := range p.keys {
		marker := " "
		if i == p.keyIdx {
			marker = ">"
		}
		rows[i] = marker + " " + k
	}
	if p.showAll {
		rows = append([]string{"> (all keys)"}, rows...)
	}
	p.keysView.Rows = rows
	p.keysView.SelectedRow = 0
	if !p.showAll && p.keyIdx >= 0 && p.keyIdx < len(p.keys) {
		p.keysView.SelectedRow = p.keyIdx
		if p.hasShowAllRow() {
			p.keysView.SelectedRow = p.keyIdx + 1
		}
	}

	title := " Value "
	if p.secret != nil {
		title = fmt.Sprintf(" %s/%s — %s ", p.secret.Namespace, p.secret.Name, p.currentKeyLabel())
	}
	p.valueView.Title = title
	p.valueView.Rows = p.valueLines()
	if p.valueView.SelectedRow >= len(p.valueView.Rows) {
		p.valueView.SelectedRow = len(p.valueView.Rows) - 1
	}
	if p.valueView.SelectedRow < 0 {
		p.valueView.SelectedRow = 0
	}
	ui.Render(p.keysView, p.valueView)
}

func (p *secretsPage) hasShowAllRow() bool { return p.showAll }

func (p *secretsPage) currentKeyLabel() string {
	if p.showAll {
		return "all keys"
	}
	if p.keyIdx < 0 || p.keyIdx >= len(p.keys) {
		return ""
	}
	label := p.keys[p.keyIdx]
	if p.decodeB64 {
		label += " (decoded)"
	} else {
		label += " (base64)"
	}
	return label
}

// valueLines returns the text to display in the value pane, split into
// lines so termui's List widget can scroll.
func (p *secretsPage) valueLines() []string {
	if p.secret == nil {
		return []string{"", "  (loading…)"}
	}
	raw := p.currentValueText()
	if raw == "" {
		return []string{"  (empty)"}
	}
	return strings.Split(raw, "\n")
}

// currentValueText returns the raw string the value pane is showing —
// either a single decoded/raw key value, or the concatenated all-keys
// dump when showAll is on. Empty if nothing is selected.
func (p *secretsPage) currentValueText() string {
	if p.secret == nil {
		return ""
	}
	if p.showAll {
		return p.allKeysText()
	}
	if p.keyIdx >= 0 && p.keyIdx < len(p.keys) {
		return p.renderValue(p.secret.Data[p.keys[p.keyIdx]])
	}
	return ""
}

// copyCurrentValue writes whatever the value pane is currently showing
// to the system clipboard and reports the result in the status banner.
func (p *secretsPage) copyCurrentValue() {
	text := p.currentValueText()
	if text == "" {
		p.showError("nothing to copy")
		return
	}
	if err := clipboard.WriteAll(text); err != nil {
		p.showError(fmt.Sprintf("clipboard: %v", err))
		return
	}
	p.showInfo(fmt.Sprintf("✓ copied %d bytes to clipboard (%s)", len(text), p.currentKeyLabel()))
}

func (p *secretsPage) showError(msg string) {
	p.errBanner.Text = msg
	p.errBanner.TextStyle = ui.NewStyle(ui.ColorRed)
}

func (p *secretsPage) showInfo(msg string) {
	p.errBanner.Text = msg
	p.errBanner.TextStyle = ui.NewStyle(ui.ColorGreen)
}

// renderValue returns the displayable form of a secret data entry. The
// kube client has already base64-decoded the on-the-wire payload to raw
// bytes, so the default view re-encodes them — that matches what users
// see in `kubectl get secret -o yaml` and makes the [b] toggle behave
// the way the help line advertises.
func (p *secretsPage) renderValue(data []byte) string {
	if p.decodeB64 {
		return string(data)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func (p *secretsPage) allKeysText() string {
	var sb strings.Builder
	for _, k := range p.keys {
		fmt.Fprintf(&sb, "## %s\n%s\n\n", k, p.renderValue(p.secret.Data[k]))
	}
	return sb.String()
}

func (p *secretsPage) help() string {
	if p.activeCluster != nil {
		mode := "base64"
		if p.decodeB64 {
			mode = "decoded"
		}
		// Two explicit lines so wrapping is deterministic — the second
		// line includes the live toggle state so the user can see what
		// pressing [b] will switch away from without having to read the
		// (possibly clipped) value pane title.
		return fmt.Sprintf(
			"[↑/↓] key  [PgUp/PgDn] page  [Home/End] top/bot  [c/Ctrl-C] copy\n[b] mode: %s  [a] show-all: %v",
			mode, p.showAll,
		)
	}
	return "[type] fuzzy search  [↑/↓] select  [PgUp/PgDn] page  [Enter] open  [Ctrl-U] clear"
}

func (p *secretsPage) handleKey(e ui.Event) bool {
	if p.activeCluster != nil {
		return p.handleDetailKey(e)
	}
	return p.handlePickerKey(e)
}

func (p *secretsPage) handlePickerKey(e ui.Event) bool {
	switch e.ID {
	case "<Escape>":
		p.app.closePage()
		return true
	case "<Up>":
		if p.selected > 0 {
			p.selected--
			p.listView.SelectedRow = p.selected
		}
		return true
	case "<Down>":
		if p.selected < len(p.filtered)-1 {
			p.selected++
			p.listView.SelectedRow = p.selected
		}
		return true
	case "<PageUp>":
		p.listView.ScrollPageUp()
		p.selected = p.listView.SelectedRow
		return true
	case "<PageDown>":
		p.listView.ScrollPageDown()
		p.selected = p.listView.SelectedRow
		return true
	case "<Enter>":
		if p.selected >= 0 && p.selected < len(p.filtered) {
			p.openCluster(p.filtered[p.selected])
		}
		return true
	case "<Backspace>", "<C-8>":
		if n := utf8.RuneCountInString(p.query); n > 0 {
			r := []rune(p.query)
			p.query = string(r[:n-1])
			p.applyFilter()
		}
		return true
	case "<C-u>":
		p.query = ""
		p.applyFilter()
		return true
	case "<Space>":
		p.query += " "
		p.applyFilter()
		return true
	}
	// Printable single-rune keys get appended to the query.
	if utf8.RuneCountInString(e.ID) == 1 {
		p.query += e.ID
		p.applyFilter()
		return true
	}
	return false
}

func (p *secretsPage) handleDetailKey(e ui.Event) bool {
	switch e.ID {
	case "<Escape>":
		p.activeCluster = nil
		p.secret = nil
		p.keys = nil
		p.keyIdx = 0
		p.decodeB64 = false
		p.showAll = false
		p.valueView.ScrollTop()
		return true
	case "<Up>":
		if p.showAll {
			p.showAll = false
			p.valueView.ScrollTop()
			return true
		}
		if p.keyIdx > 0 {
			p.keyIdx--
			p.valueView.ScrollTop()
		}
		return true
	case "<Down>":
		if p.keyIdx < len(p.keys)-1 {
			p.keyIdx++
			p.valueView.ScrollTop()
		}
		return true
	case "b", "B":
		p.decodeB64 = !p.decodeB64
		p.valueView.ScrollTop()
		return true
	case "a", "A":
		p.showAll = !p.showAll
		p.valueView.ScrollTop()
		return true
	case "<PageUp>":
		p.valueView.ScrollPageUp()
		return true
	case "<PageDown>":
		p.valueView.ScrollPageDown()
		return true
	case "<Home>":
		p.valueView.ScrollTop()
		return true
	case "<End>":
		p.valueView.ScrollBottom()
		return true
	case "c", "C", "<C-c>":
		p.copyCurrentValue()
		return true
	}
	return false
}

func (p *secretsPage) openCluster(c clusterEntry) {
	p.activeCluster = &c
	p.secret = nil
	p.keys = nil
	p.keyIdx = 0
	p.decodeB64 = false
	p.showAll = false

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	secret, err := extract.FindClusterSecret(ctx, c.client.Ctrl, c.cluster)
	if err != nil {
		p.showError(fmt.Sprintf("fetching secret: %v", err))
		// Remain on detail view but with an empty secret so user can Esc back.
		return
	}
	p.secret = secret
	p.keys = make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		p.keys = append(p.keys, k)
	}
	sort.Strings(p.keys)
	p.errBanner.Text = ""
}
