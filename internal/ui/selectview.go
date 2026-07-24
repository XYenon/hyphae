package ui

import (
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/aleksanaa/hyphae/internal/strutil"
)

// SelectView is a pick-one prompt shown when the agent calls ask_user.
// Height varies with option count; use SelectViewHeight to compute it.
type SelectView struct {
	*tview.Box
	question string
	options  []string
	cursor   int // 0..len(options); len(options) == custom-text row
	// clickArmed gates confirm-on-double-click to the row the double-click's own
	// first click landed on; see the identical note in ApprovalView.
	clickArmed  bool
	customField *tview.InputField
	visible     bool
	onSubmit    func(string)
}

func NewSelectView() *SelectView {
	sv := &SelectView{Box: tview.NewBox()}
	sv.Box.SetBackgroundColor(Theme.Surface)
	sv.SetBorder(true)
	sv.SetBorderColor(Theme.PendingColor)
	sv.SetTitleColor(Theme.PendingColor)
	sv.SetTitleAlign(tview.AlignLeft)
	sv.SetTitle(" apex is asking ")

	sv.customField = tview.NewInputField()
	sv.customField.SetPlaceholder("tell apex what to do instead...")
	sv.customField.SetFieldTextColor(Theme.Text)
	sv.customField.SetFieldBackgroundColor(Theme.Surface)
	sv.customField.SetBackgroundColor(Theme.Surface)
	sv.customField.SetDoneFunc(func(key tcell.Key) {
		if key == tcell.KeyEnter {
			sv.confirm()
		}
	})
	return sv
}

// Restyle re-applies theme colors after a theme switch. The custom-field
// highlight state is re-derived on the next render.
func (sv *SelectView) Restyle() {
	sv.Box.SetBackgroundColor(Theme.Surface)
	sv.SetBorderColor(Theme.PendingColor)
	sv.SetTitleColor(Theme.PendingColor)
	sv.customField.SetFieldTextColor(Theme.Text)
	sv.customField.SetFieldBackgroundColor(Theme.Surface)
	sv.customField.SetBackgroundColor(Theme.Surface)
}

func (sv *SelectView) IsVisible() bool { return sv.visible }

// Height returns the row count needed to render the view at availWidth columns.
// Call after Show so the question is set.
func (sv *SelectView) Height(availWidth int) int {
	innerW := max(1, availWidth-4)
	qLines := len(tview.WordWrap(tview.Escape(sv.question), innerW))
	if qLines == 0 {
		qLines = 1
	}
	return 2 + qLines + 1 + len(sv.options) + 1 // borders + question + blank + options + custom
}

func (sv *SelectView) Show(question string, options []string) {
	sv.question = question
	sv.options = options
	sv.cursor = 0
	sv.clickArmed = false
	sv.customField.SetText("")
	sv.visible = true
}

func (sv *SelectView) SetCallback(fn func(string)) { sv.onSubmit = fn }

func (sv *SelectView) confirm() {
	if !sv.visible || sv.onSubmit == nil {
		return
	}
	if sv.cursor < len(sv.options) {
		sv.onSubmit(sv.options[sv.cursor])
	} else if text := sv.customField.GetText(); text != "" {
		sv.onSubmit(text)
	}
}

// Cancel sends a sentinel reply so the agent is not left hanging.
func (sv *SelectView) Cancel() {
	if sv.onSubmit != nil {
		sv.onSubmit("(user dismissed without selecting)")
	}
}

// Focus delegates cursor focus to customField when it is the active row.
func (sv *SelectView) Focus(delegate func(p tview.Primitive)) {
	sv.Box.Focus(delegate)
	if sv.cursor == len(sv.options) {
		sv.customField.Focus(func(tview.Primitive) {})
	} else {
		sv.customField.Blur()
	}
}

// ── Draw ─────────────────────────────────────────────────────────────────────

func (sv *SelectView) Draw(screen tcell.Screen) {
	sv.Box.DrawForSubclass(screen, sv)
	if !sv.visible {
		return
	}
	x, y, w, h := sv.GetRect()
	if w < 20 || h < 4 {
		return
	}
	inner := x + 2
	innerW := w - 4
	bg := Theme.Surface

	mutedSt := tcell.StyleDefault.Foreground(Theme.Muted).Background(bg)
	textSt := tcell.StyleDefault.Foreground(Theme.Text).Background(bg)

	// Row 1+: question, word-wrapped (escape so [ chars don't confuse WordWrap).
	qLines := tview.WordWrap(tview.Escape(sv.question), innerW)
	if len(qLines) == 0 {
		qLines = []string{""}
	}
	for j, line := range qLines {
		drawText(screen, tview.Unescape(line), inner, y+1+j, innerW, textSt)
	}

	// Rows after question + 1 blank line: option rows.
	optStart := y + 1 + len(qLines) + 1
	total := len(sv.options) + 1 // includes custom-text row
	for i := 0; i < total && optStart+i < y+h-1; i++ {
		row := optStart + i
		isSelected := i == sv.cursor

		var st tcell.Style
		if isSelected {
			st = tcell.StyleDefault.Foreground(Theme.Text).Background(selectHighlightBg)
			for col := x + 1; col < x+w-1; col++ {
				screen.SetContent(col, row, ' ', nil, st)
			}
		} else {
			st = mutedSt
		}

		bullet := "○ "
		if isSelected {
			bullet = "● "
		}
		col := inner
		bulletW := drawText(screen, bullet, col, row, innerW, st)
		col += bulletW
		remaining := innerW - bulletW

		if i < len(sv.options) {
			// Option text is light even when not selected; only the bullet is muted.
			optSt := textSt
			if isSelected {
				optSt = st
			}
			drawText(screen, strutil.Truncate(sv.options[i], remaining), col, row, remaining, optSt)
		} else {
			fieldW := remaining
			if fieldW > 0 {
				if isSelected {
					sv.customField.SetFieldBackgroundColor(selectHighlightBg)
					sv.customField.SetFieldTextColor(Theme.Text)
					sv.customField.SetPlaceholderStyle(
						tcell.StyleDefault.Foreground(Theme.Muted).Background(selectHighlightBg))
					sv.customField.SetBackgroundColor(selectHighlightBg)
				} else {
					sv.customField.SetFieldBackgroundColor(bg)
					sv.customField.SetFieldTextColor(Theme.Text) // typed text matches option brightness
					sv.customField.SetPlaceholderStyle(
						tcell.StyleDefault.Foreground(Theme.Muted).Background(bg))
					sv.customField.SetBackgroundColor(bg)
				}
				sv.customField.SetRect(col, row, fieldW, 1)
				sv.customField.Draw(screen)
			}
		}
	}
}

// ── InputHandler ─────────────────────────────────────────────────────────────

func (sv *SelectView) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return sv.WrapInputHandler(func(event *tcell.EventKey, setFocus func(tview.Primitive)) {
		if !sv.visible {
			return
		}
		switch event.Key() {
		case tcell.KeyUp:
			if sv.cursor > 0 {
				sv.cursor--
			}
			if sv.cursor == len(sv.options) {
				sv.customField.Focus(func(tview.Primitive) {})
			} else {
				sv.customField.Blur()
			}
			setFocus(sv)
		case tcell.KeyDown:
			if sv.cursor < len(sv.options) {
				sv.cursor++
			}
			if sv.cursor == len(sv.options) {
				sv.customField.Focus(func(tview.Primitive) {})
			} else {
				sv.customField.Blur()
			}
			setFocus(sv)
		case tcell.KeyEnter:
			sv.confirm()
		default:
			// Forward all other input to customField when it is the active row.
			if sv.cursor == len(sv.options) {
				if h := sv.customField.InputHandler(); h != nil {
					h(event, setFocus)
				}
			}
		}
	})
}

// PasteHandler forwards bracketed-paste text to the custom-answer field,
// matching InputHandler's rune routing: paste only lands when the custom row is
// the active cursor. Box's default PasteHandler drops it otherwise.
func (sv *SelectView) PasteHandler() func(string, func(tview.Primitive)) {
	return sv.WrapPasteHandler(func(pastedText string, setFocus func(tview.Primitive)) {
		if !sv.visible || sv.cursor != len(sv.options) {
			return
		}
		if h := sv.customField.PasteHandler(); h != nil {
			h(pastedText, setFocus)
		}
	})
}

// ── MouseHandler ─────────────────────────────────────────────────────────────

func (sv *SelectView) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return sv.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(tview.Primitive)) (bool, tview.Primitive) {
		if !sv.visible {
			return false, nil
		}
		mx, my := event.Position()
		// A click outside the prompt must fall through so a later sibling (input,
		// chat) can take it — mirror tview's default Box.MouseHandler InRect gate.
		if !sv.InRect(mx, my) {
			return false, nil
		}
		// A left-click anywhere in the prompt grabs focus, so it becomes keyboard-
		// active even when an option row isn't the target.
		grabFocus := func() (bool, tview.Primitive) {
			switch action {
			case tview.MouseLeftDown, tview.MouseLeftClick, tview.MouseLeftDoubleClick:
				sv.clickArmed = false // a click off the option rows disarms confirm
				setFocus(sv)
				return true, nil
			}
			return false, nil
		}

		_, y, _, _ := sv.GetRect()

		optRow := my - (y + 3)
		total := len(sv.options) + 1
		if optRow < 0 || optRow >= total {
			return grabFocus()
		}

		switch action {
		case tview.MouseLeftDown:
			setFocus(sv)
			return true, nil
		case tview.MouseLeftClick:
			sv.cursor = optRow
			if sv.cursor == len(sv.options) {
				sv.customField.Focus(func(tview.Primitive) {})
			} else {
				sv.customField.Blur()
			}
			sv.clickArmed = true
			setFocus(sv)
			return true, nil
		case tview.MouseLeftDoubleClick:
			// Confirm only when this double-click's own first click already
			// selected this same row; otherwise treat it as a plain select
			// (a fast cross-row tap tview merged into a double-click).
			if sv.clickArmed && sv.cursor == optRow {
				if sv.cursor == len(sv.options) {
					sv.customField.Focus(func(tview.Primitive) {})
				}
				sv.confirm()
				return true, nil
			}
			sv.cursor = optRow
			if sv.cursor == len(sv.options) {
				sv.customField.Focus(func(tview.Primitive) {})
			} else {
				sv.customField.Blur()
			}
			sv.clickArmed = true
			setFocus(sv)
			return true, nil
		}
		return grabFocus()
	})
}
