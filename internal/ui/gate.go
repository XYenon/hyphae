package ui

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Minimum terminal size below which the UI refuses to render, showing a notice
// instead. Chosen so the layout (tabs + chat + status bar + palette) stays legible.
const (
	minScreenW = 60
	minScreenH = 26
)

// minSizeGate wraps the root primitive and only renders it when the terminal is
// at least minScreenW × minScreenH. Below that it draws a centered notice, and
// once the window grows back it renders normally again.
type minSizeGate struct {
	*tview.Box
	inner tview.Primitive
}

func newMinSizeGate(inner tview.Primitive) *minSizeGate {
	return &minSizeGate{Box: tview.NewBox(), inner: inner}
}

func (g *minSizeGate) tooSmall() (int, int, bool) {
	_, _, w, h := g.GetRect()
	return w, h, w < minScreenW || h < minScreenH
}

func (g *minSizeGate) Draw(screen tcell.Screen) {
	x, y, w, h := g.GetRect()
	if w <= 0 || h <= 0 {
		return
	}
	if w < minScreenW || h < minScreenH {
		mid := y + h/2
		title := fmt.Sprintf("[%s::b]terminal too small[-:-:-]", TC.PendingColor)
		detail := fmt.Sprintf("[%s]minimum %d × %d — now %d × %d[-]", TC.Muted, minScreenW, minScreenH, w, h)
		tview.Print(screen, title, x, mid-1, w, tview.AlignCenter, Theme.Text)
		tview.Print(screen, detail, x, mid+1, w, tview.AlignCenter, Theme.Text)
		return
	}
	g.inner.SetRect(x, y, w, h)
	g.inner.Draw(screen)
}

func (g *minSizeGate) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return g.WrapMouseHandler(func(action tview.MouseAction, event *tcell.EventMouse, setFocus func(tview.Primitive)) (bool, tview.Primitive) {
		if _, _, small := g.tooSmall(); small {
			return true, nil // swallow input while the notice is showing
		}
		return g.inner.MouseHandler()(action, event, setFocus)
	})
}

func (g *minSizeGate) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return func(event *tcell.EventKey, setFocus func(tview.Primitive)) {
		if _, _, small := g.tooSmall(); small {
			return
		}
		if h := g.inner.InputHandler(); h != nil {
			h(event, setFocus)
		}
	}
}

// PasteHandler delegates bracketed-paste events to the wrapped primitive.
// Box.PasteHandler is a no-op, so without this the gate (being the root) would
// silently drop every paste once EnablePaste is on.
func (g *minSizeGate) PasteHandler() func(string, func(tview.Primitive)) {
	return func(pastedText string, setFocus func(tview.Primitive)) {
		if _, _, small := g.tooSmall(); small {
			return
		}
		if h := g.inner.PasteHandler(); h != nil {
			h(pastedText, setFocus)
		}
	}
}

// Focus, Blur, and HasFocus delegate to the wrapped primitive. tview only routes
// keyboard events to the root when root.HasFocus() is true, so without this the
// gate (being the root) would swallow all input even at a valid size.
func (g *minSizeGate) Focus(delegate func(p tview.Primitive)) {
	g.inner.Focus(delegate)
}

func (g *minSizeGate) Blur() {
	g.inner.Blur()
}

func (g *minSizeGate) HasFocus() bool {
	return g.inner.HasFocus()
}
