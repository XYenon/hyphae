package ui

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	"github.com/rivo/uniseg"
)

// minApprovalHeight is the smallest row count the approval bar renders at (border
// + one content line + the buttons row). Taller when args/reasoning wrap; the
// live height is computed by Height.
const minApprovalHeight = 5

// Per-field wrap caps, so a pathological argument (e.g. a giant blob that slipped
// past the diff path) can't grow the bar without bound and swallow the chat.
const (
	maxArgValueLines  = 8
	maxReasoningLines = 12
)

// argLine is one key/value pair displayed in the approval bar, in the order the
// bar shows them (see sortArgKeys).
type argLine struct {
	key   string
	value string
}

// ApprovalView is a confirmation bar for tool calls that don't produce a diff.
// It sits between the chat and input in the layout. When hidden its height is 0.
// It renders the call's arguments one per line (each wrapping as needed) plus the
// model's reasoning (also wrapped); diff-producing calls go to DiffView instead.
type ApprovalView struct {
	*tview.Box
	toolName  string
	args      []argLine
	reasoning string
	selected  string // "allow" | "deny"
	// clickArmed records that the last mouse event was a single-click on the
	// currently-selected button, so a following double-click can only confirm the
	// same button its own first click landed on. tview reports two fast clicks on
	// different targets as a double-click (its detection ignores position), which
	// on a touchscreen could otherwise approve/deny unintentionally.
	clickArmed bool

	// deny text is managed by a native InputField (handles cursor, CJK, wide chars).
	denyField *tview.InputField

	visible bool
	onAllow func()
	onDeny  func(string)
}

func NewApprovalView() *ApprovalView {
	av := &ApprovalView{
		Box:      tview.NewBox(),
		selected: "allow",
	}
	av.Box.SetBackgroundColor(Theme.Surface)
	av.SetBorder(true)
	av.SetBorderColor(Theme.PendingColor)
	av.SetTitleColor(Theme.PendingColor)
	av.SetTitleAlign(tview.AlignLeft)

	av.denyField = tview.NewInputField()
	av.denyField.SetPlaceholder("type reason here (optional)...")
	av.denyField.SetFieldTextColor(Theme.Text)
	av.denyField.SetFieldBackgroundColor(Theme.Surface)
	av.denyField.SetBackgroundColor(Theme.Surface)
	av.denyField.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			av.confirm()
		}
	})

	return av
}

// Restyle re-applies theme colors after a theme switch. Deny-field state colors
// are re-derived on the next render from the current mode.
func (av *ApprovalView) Restyle() {
	av.Box.SetBackgroundColor(Theme.Surface)
	av.SetBorderColor(Theme.PendingColor)
	av.SetTitleColor(Theme.PendingColor)
	av.denyField.SetFieldTextColor(Theme.Text)
	av.denyField.SetFieldBackgroundColor(Theme.Surface)
	av.denyField.SetBackgroundColor(Theme.Surface)
}

// ── Focus delegation ─────────────────────────────────────────────────────────

// Focus keeps hasFocus on the approval bar itself (so its InputHandler fires
// for Left/Right/Enter) and separately sets hasFocus on denyField (for cursor)
// when deny mode is active.
func (av *ApprovalView) Focus(delegate func(p tview.Primitive)) {
	av.Box.Focus(delegate) // sets av.Box.hasFocus = true
	if av.selected == "deny" {
		noop := func(tview.Primitive) {}
		av.denyField.Focus(noop) // sets denyField.Box.hasFocus for cursor
	} else {
		av.denyField.Blur()
	}
}

// ── public API ───────────────────────────────────────────────────────────────

func (av *ApprovalView) IsVisible() bool      { return av.visible }
func (av *ApprovalView) GetSelected() string  { return av.selected }
func (av *ApprovalView) SetSelected(s string) { av.selected = s }

// Show populates the bar from a tool call's structured arguments and reasoning
// (reasoning already excluded from args upstream); every field is listed one per
// line in sortArgKeys order.
func (av *ApprovalView) Show(toolName string, args map[string]any, reasoning string) {
	av.toolName = toolName
	av.SetTitle(" " + toolName + " ")
	av.args = av.args[:0]
	for _, k := range sortArgKeys(args) {
		av.args = append(av.args, argLine{key: k, value: formatArgValue(args[k])})
	}
	av.reasoning = reasoning
	av.selected = "allow"
	av.clickArmed = false
	av.denyField.SetText("")
	av.visible = true
}

// argKeyRank orders the common primary arguments first so the most relevant field
// leads; everything else follows alphabetically (see sortArgKeys).
var argKeyRank = map[string]int{
	"command": 0, "url": 1, "query": 2, "path_glob": 3,
	"path": 4, "type": 5, "target": 6,
}

// sortArgKeys returns the arg keys in display order: ranked primaries first, then
// the rest alphabetically. Deterministic, so the bar doesn't reshuffle on redraw.
func sortArgKeys(args map[string]any) []string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, oki := argKeyRank[keys[i]]
		rj, okj := argKeyRank[keys[j]]
		if oki != okj {
			return oki
		}
		if oki && ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// formatArgValue renders an argument value for display: strings as-is, everything
// else (numbers, bools, lists) as compact JSON so it stays on readable lines.
func formatArgValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", v)
}

// Height returns the row count needed to render the bar at availWidth columns:
// borders + the wrapped arg/reasoning lines + the buttons row. Call after Show.
func (av *ApprovalView) Height(availWidth int) int {
	innerW := max(1, availWidth-4)
	content := av.contentLineCount(innerW)
	return content + 3 // top+bottom border + buttons row
}

// contentLineCount is the total wrapped-line count of the args and reasoning at
// innerW columns (at least 1, so an empty bar still reserves a content row).
func (av *ApprovalView) contentLineCount(innerW int) int {
	n := 0
	for _, a := range av.args {
		n += len(wrapFieldLines(a.key, a.value, innerW, maxArgValueLines))
	}
	if av.reasoning != "" {
		n += len(wrapFieldLines("reason", av.reasoning, innerW, maxReasoningLines))
	}
	if n == 0 {
		n = 1
	}
	return n
}

// wrapFieldLines word-wraps value into lines sized for a "<key> ❯ " prefix
// (continuation lines hang-indent under the value), capped at maxLines with an
// ellipsis on the last kept line when it overflows. Shared by the approval bar
// and the diff view so both render args/reasoning identically.
func wrapFieldLines(key, value string, innerW, maxLines int) []string {
	prefixW := uniseg.StringWidth(key + " ❯ ")
	valW := max(1, innerW-prefixW)
	lines := tview.WordWrap(tview.Escape(value), valW)
	if len(lines) == 0 {
		lines = []string{""}
	}
	for i := range lines {
		lines[i] = tview.Unescape(lines[i])
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines[maxLines-1] = truncateToWidth(lines[maxLines-1], valW-1) + "…"
	}
	return lines
}

// drawWrappedField draws a "<key> ❯ <value>" field starting at row, wrapping the
// value with a hanging indent under it (see wrapFieldLines). Rows at or past
// limit are clipped. Returns the next free row.
func drawWrappedField(screen tcell.Screen, key, value string, inner, row, limit, innerW, maxLines int, keySt, valSt tcell.Style) int {
	prefix := key + " ❯ "
	prefixW := uniseg.StringWidth(prefix)
	for j, ln := range wrapFieldLines(key, value, innerW, maxLines) {
		if row >= limit {
			break
		}
		if j == 0 {
			drawText(screen, prefix, inner, row, innerW, keySt)
		}
		drawText(screen, ln, inner+prefixW, row, innerW-prefixW, valSt)
		row++
	}
	return row
}

// truncateToWidth clips s to at most w display columns (no ellipsis added).
func truncateToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	used := 0
	out := make([]rune, 0, len(s))
	for _, r := range s {
		rw := uniseg.StringWidth(string(r))
		if rw == 0 {
			rw = 1
		}
		if used+rw > w {
			break
		}
		out = append(out, r)
		used += rw
	}
	return string(out)
}

func (av *ApprovalView) SetCallbacks(onAllow func(), onDeny func(string)) {
	av.onAllow = onAllow
	av.onDeny = onDeny
}

func (av *ApprovalView) Allow() {
	if av.onAllow != nil {
		av.onAllow()
	}
}

func (av *ApprovalView) Deny(reason string) {
	if av.onDeny != nil {
		av.onDeny(reason)
	}
}

func (av *ApprovalView) confirm() {
	if av.selected == "allow" {
		av.Allow()
	} else {
		av.Deny(av.denyField.GetText())
	}
}

// ── Draw ─────────────────────────────────────────────────────────────────────

func (av *ApprovalView) Draw(screen tcell.Screen) {
	av.Box.DrawForSubclass(screen, av)
	if !av.visible {
		return
	}
	x, y, w, h := av.GetRect()
	if w < 20 || h < minApprovalHeight {
		return
	}

	bg := Theme.Surface
	mutedSt := tcell.StyleDefault.Foreground(Theme.Muted).Background(bg)
	textSt := tcell.StyleDefault.Foreground(Theme.Text).Background(bg)
	shellSt := tcell.StyleDefault.Foreground(Theme.ShellColor).Background(bg)

	inner := x + 2
	innerW := w - 4
	buttonRow := y + h - 2 // buttons pinned to the last inner row

	// ── args + reasoning: each field one per line, wrapping as needed ──────
	row := y + 1
	for _, a := range av.args {
		row = drawWrappedField(screen, a.key, a.value, inner, row, buttonRow, innerW, maxArgValueLines, mutedSt, shellSt)
	}
	if av.reasoning != "" {
		drawWrappedField(screen, "reason", av.reasoning, inner, row, buttonRow, innerW, maxReasoningLines, mutedSt, textSt)
	}

	// ── buttons: [ Allow ]   [ Deny: <denyField> ] ────────────────────────
	allowSt := tcell.StyleDefault.Foreground(Theme.SuccessColor).Background(bg)
	if av.selected == "allow" {
		allowSt = tcell.StyleDefault.Background(approvalDarkGreen).Foreground(approvalWhite)
	}
	col := inner
	col += drawText(screen, "[ Allow ]", col, buttonRow, innerW, allowSt)

	// gap
	col = inner + 9 + 3

	denyEnd := x + w - 2
	denyW := denyEnd - col
	if denyW >= 10 {
		denyBracketSt := tcell.StyleDefault.Foreground(Theme.ErrorColor).Background(bg)
		if av.selected == "deny" {
			denyBracketSt = tcell.StyleDefault.Background(approvalDarkRed).Foreground(approvalWhite)
		}

		const denyPfx = "[ Deny: "
		const denySfx = " ]"
		col += drawText(screen, denyPfx, col, buttonRow, denyW, denyBracketSt)

		textAreaW := denyEnd - col - len([]rune(denySfx))
		if textAreaW > 0 {
			if av.selected == "deny" {
				av.denyField.SetFieldBackgroundColor(approvalDarkRed)
				av.denyField.SetFieldTextColor(approvalWhite)
				av.denyField.SetPlaceholderStyle(
					tcell.StyleDefault.Foreground(Theme.Muted).Background(approvalDarkRed))
			} else {
				av.denyField.SetFieldBackgroundColor(bg)
				av.denyField.SetFieldTextColor(Theme.Muted)
				av.denyField.SetPlaceholderStyle(
					tcell.StyleDefault.Foreground(Theme.Muted).Background(bg))
			}
			av.denyField.SetBackgroundColor(bg)
			av.denyField.SetRect(col, buttonRow, textAreaW, 1)
			av.denyField.Draw(screen)
			col += textAreaW
		}
		drawText(screen, denySfx, col, buttonRow, len([]rune(denySfx)), denyBracketSt)
	}

}

// ── InputHandler ─────────────────────────────────────────────────────────────

func (av *ApprovalView) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return av.WrapInputHandler(func(event *tcell.EventKey, setFocus func(tview.Primitive)) {
		if !av.visible {
			return
		}
		switch event.Key() {
		case tcell.KeyLeft:
			av.selected = "allow"
			av.denyField.Blur()
			setFocus(av)
		case tcell.KeyRight:
			av.selected = "deny"
			av.denyField.Focus(func(tview.Primitive) {})
			setFocus(av)
		case tcell.KeyEnter:
			av.confirm()
		default:
			// Route all other input (runes, backspace, etc.) to denyField
			// when deny mode is active.
			if av.selected == "deny" {
				if h := av.denyField.InputHandler(); h != nil {
					h(event, setFocus)
				}
			}
		}
	})
}

// PasteHandler forwards bracketed-paste text to the deny-reason field, matching
// InputHandler's rune routing: paste only lands when deny mode is active. Box's
// default PasteHandler drops it, so this is required for pasting a reason.
func (av *ApprovalView) PasteHandler() func(string, func(tview.Primitive)) {
	return av.WrapPasteHandler(func(pastedText string, setFocus func(tview.Primitive)) {
		if !av.visible || av.selected != "deny" {
			return
		}
		if h := av.denyField.PasteHandler(); h != nil {
			h(pastedText, setFocus)
		}
	})
}

// ── MouseHandler ─────────────────────────────────────────────────────────────

func (av *ApprovalView) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return av.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(tview.Primitive)) (bool, tview.Primitive) {
		if !av.visible {
			return false, nil
		}
		mx, my := event.Position()
		// A click outside the bar must fall through so a later sibling (input,
		// chat) can take it — mirror tview's default Box.MouseHandler InRect gate.
		if !av.InRect(mx, my) {
			return false, nil
		}
		// A left-click anywhere in the bar grabs focus, so it becomes keyboard-
		// active even when the buttons themselves aren't the target.
		grabFocus := func() (bool, tview.Primitive) {
			switch action {
			case tview.MouseLeftDown, tview.MouseLeftClick, tview.MouseLeftDoubleClick:
				av.clickArmed = false // a click off the buttons disarms confirm
				setFocus(av)
				return true, nil
			}
			return false, nil
		}

		x, y, w, h := av.GetRect()

		if my != y+h-2 { // buttons live on the last inner row
			return grabFocus()
		}

		inner := x + 2
		allowEnd := inner + 9
		denyStart := inner + 9 + 3
		denyEnd := x + w - 2

		var side string
		switch {
		case mx >= inner && mx < allowEnd:
			side = "allow"
		case mx >= denyStart && mx < denyEnd:
			side = "deny"
		default:
			return grabFocus()
		}

		switch action {
		case tview.MouseLeftDown:
			setFocus(av)
			return true, nil
		case tview.MouseLeftClick:
			av.selected = side
			av.clickArmed = true
			setFocus(av)
			return true, nil
		case tview.MouseLeftDoubleClick:
			// Confirm only when this double-click's own first click already
			// selected this same button; otherwise treat it as a plain select
			// (a fast cross-target tap tview merged into a double-click).
			if av.clickArmed && av.selected == side {
				av.confirm()
				return true, nil
			}
			av.selected = side
			av.clickArmed = true
			setFocus(av)
			return true, nil
		}
		return grabFocus()
	})
}

func drawText(screen tcell.Screen, text string, col, row, maxCols int, st tcell.Style) int {
	used := 0
	for _, r := range text {
		rw := uniseg.StringWidth(string(r))
		if rw == 0 {
			rw = 1
		}
		if used+rw > maxCols {
			break
		}
		screen.SetContent(col+used, row, r, nil, st)
		used += rw
	}
	return used
}
