package topview

import (
	"io"
	"time"

	"mellomting/internal/topview/term"
	"mellomting/internal/usagehist"
)

// View is one screen of top.
type View int

// Views, in tab order; the number key selects each (1 is Overview).
const (
	Overview View = iota
	Keys
	Models
	Backends
	Requests
	Usage
	numViews
)

var viewNames = [numViews]string{"Overview", "Keys", "Models", "Backends", "Requests", "Usage"}

func (v View) String() string {
	if v >= 0 && v < numViews {
		return viewNames[v]
	}
	return "?"
}

// Options configure Run and Plain.
type Options struct {
	Redact   bool
	NoColor  bool
	Interval time.Duration
	// Rows bounds the requests Plain prints; 0 prints all.
	Rows int
	// Usage is the accounting history, or nil with UsageErr saying why.
	Usage    *usagehist.History
	UsageErr string
	// BackendServer maps generated backend names to their server.
	BackendServer map[string]string
	// In is the terminal's input.
	In  io.Reader
	Loc *time.Location
}

// sortModes are each view's sort orders; s cycles them. "name" sorts
// ascending, everything else largest first.
var sortModes = [numViews][]string{
	Keys:     {"refused", "inflight", "req/s", "tokens", "name"},
	Models:   {"req/s", "tokens", "errors", "name"},
	Backends: {"load", "req/s", "errors", "name"},
	Requests: {"age", "phase", "key", "bytes"},
}

func sortIndex(v View, name string) int {
	for i, n := range sortModes[v] {
		if n == name {
			return i
		}
	}
	return 0
}

var usageDims = [...]string{"keys", "models", "servers"}

// UIState is what the operator has chosen: view, selection, sort,
// filter. It holds no data; Frame draws a Model through it.
type UIState struct {
	View View
	// Sel is each view's selected item, by ID, so it follows the item
	// as rows reorder.
	Sel       [numViews]string
	Detail    bool
	Sort      [numViews]int
	Filter    string
	Filtering bool
	Redact    bool
	Paused    bool
	NoColor   bool
	UsageDim  int
	// Height is the terminal height of the last frame; it sets page
	// size and how many items Overview lists.
	Height int
	Quit   bool
}

// NewUI opens on the Overview dashboard.
func NewUI(opt Options) UIState {
	return UIState{View: Overview, Redact: opt.Redact, NoColor: opt.NoColor, Height: 40}
}

const maxFilter = 40

// handle applies one key press.
func (ui *UIState) handle(k term.Key, m *Model) {
	if k.Code == term.CtrlC {
		ui.Quit = true
		return
	}
	if ui.Filtering {
		switch k.Code {
		case term.Rune:
			if len([]rune(ui.Filter)) < maxFilter {
				ui.Filter += string(k.Rune)
			}
		case term.Backspace:
			if r := []rune(ui.Filter); len(r) > 0 {
				ui.Filter = string(r[:len(r)-1])
			}
		case term.Enter:
			ui.Filtering = false
		case term.Esc:
			ui.Filtering, ui.Filter = false, ""
		}
		return
	}
	switch k.Code {
	case term.Up:
		ui.move(m, -1)
	case term.Down:
		ui.move(m, 1)
	case term.PgUp:
		ui.move(m, -ui.page())
	case term.PgDn:
		ui.move(m, ui.page())
	case term.Home:
		ui.move(m, -1<<30)
	case term.End:
		ui.move(m, 1<<30)
	case term.Enter:
		if len(viewRows(m, ui)) > 0 {
			ui.Detail = true
		}
	case term.Esc:
		if ui.Filter != "" {
			ui.Filter = ""
		} else {
			ui.Detail = false
		}
	case term.Left:
		ui.setView((ui.View + numViews - 1) % numViews)
	case term.Right, term.Tab:
		ui.setView((ui.View + 1) % numViews)
	case term.Rune:
		switch r := k.Rune; {
		case r >= '1' && r < '1'+rune(numViews):
			ui.setView(View(r - '1'))
		case r == 'k':
			ui.move(m, -1)
		case r == 'j':
			ui.move(m, 1)
		case r == 'q':
			ui.Quit = true
		case r == 's':
			if ui.View == Usage {
				ui.UsageDim = (ui.UsageDim + 1) % len(usageDims)
			} else if n := len(sortModes[ui.View]); n > 0 {
				ui.Sort[ui.View] = (ui.Sort[ui.View] + 1) % n
			}
		case r == '/':
			ui.Filtering, ui.Filter = true, ""
		case r == 'r':
			ui.Redact = !ui.Redact
		case r == 'p':
			ui.Paused = !ui.Paused
		}
	}
}

func (ui *UIState) setView(v View) {
	if v != ui.View {
		ui.View, ui.Detail, ui.Filter = v, false, ""
	}
}

func (ui *UIState) page() int { return max(ui.Height-14, 5) }

func (ui *UIState) move(m *Model, by int) {
	rows := viewRows(m, ui)
	if len(rows) == 0 {
		return
	}
	i := min(max(selIndex(rows, ui.Sel[ui.View])+by, 0), len(rows)-1)
	ui.Sel[ui.View] = rows[i].id
}

// selIndex is the selected row's index, or 0 when the item is gone.
func selIndex(rows []row, id string) int {
	for i, r := range rows {
		if r.id == id {
			return i
		}
	}
	return 0
}
