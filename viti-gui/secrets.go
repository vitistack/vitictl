package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
		p.errBanner.Text = fmt.Sprintf("reading availability zones: %v", err)
		return
	}
	if len(zones) == 0 {
		p.errBanner.Text = "no availability zones configured — run `viti config init`"
		return
	}

	var warnings []string
	clients, err := kube.ConnectAll(ctx, zones, true, func(werr error) {
		warnings = append(warnings, werr.Error())
	})
	if err != nil {
		p.errBanner.Text = fmt.Sprintf("connect: %v", err)
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
		p.errBanner.Text = "warnings: " + strings.Join(warnings, "; ")
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

	p.search.Text = " " + p.query + "▏"
	p.search.SetRect(x0, y0, x1, y0+searchH)
	p.listView.SetRect(x0, y0+searchH, x1, y1-errH)
	ui.Render(p.search, p.listView)
	if errH > 0 {
		p.errBanner.SetRect(x0, y1-errH, x1, y1)
		ui.Render(p.errBanner)
	}
}

func (p *secretsPage) renderDetail() {
	x0, y0, x1, y1 := p.rect.x0, p.rect.y0, p.rect.x1, p.rect.y1
	errH := 0
	if p.errBanner.Text != "" {
		errH = 1
	}

	// Split right region: 30% keys, 70% value.
	split := x0 + (x1-x0)/3
	if split-x0 < 20 {
		split = x0 + 20
	}
	p.keysView.SetRect(x0, y0, split, y1-errH)
	p.valueView.SetRect(split, y0, x1, y1-errH)

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
	ui.Render(p.keysView, p.valueView)
	if errH > 0 {
		p.errBanner.SetRect(x0, y1-errH, x1, y1)
		ui.Render(p.errBanner)
	}
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
		label += " (b64→decoded)"
	}
	return label
}

// valueLines returns the text to display in the value pane, split into
// lines so termui's List widget can scroll.
func (p *secretsPage) valueLines() []string {
	if p.secret == nil {
		return []string{"", "  (loading…)"}
	}
	var raw string
	if p.showAll {
		raw = p.allKeysText()
	} else if p.keyIdx >= 0 && p.keyIdx < len(p.keys) {
		data := p.secret.Data[p.keys[p.keyIdx]]
		raw = p.renderValue(data)
	}
	if raw == "" {
		return []string{"  (empty)"}
	}
	return strings.Split(raw, "\n")
}

func (p *secretsPage) renderValue(data []byte) string {
	if p.decodeB64 {
		if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data))); err == nil {
			return string(decoded)
		}
		return fmt.Sprintf("[value is not valid base64]\n\n%s", string(data))
	}
	return string(data)
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
		return "[↑/↓] key   [b] decode base64   [a] toggle show-all"
	}
	return "[type] fuzzy search   [↑/↓] select   [enter] open   [ctrl-u] clear"
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
		return true
	case "<Up>":
		if p.showAll {
			p.showAll = false
			return true
		}
		if p.keyIdx > 0 {
			p.keyIdx--
			p.decodeB64 = false
		}
		return true
	case "<Down>":
		if p.keyIdx < len(p.keys)-1 {
			p.keyIdx++
			p.decodeB64 = false
		}
		return true
	case "b", "B":
		p.decodeB64 = !p.decodeB64
		return true
	case "a", "A":
		p.showAll = !p.showAll
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
		p.errBanner.Text = fmt.Sprintf("fetching secret: %v", err)
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
