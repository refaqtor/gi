// Copyright (c) 2018, The GoKi Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"github.com/goki/gi/spell"

	"github.com/goki/gi/gi"
	"github.com/goki/gi/gimain"
	"github.com/goki/gi/giv"
	"github.com/goki/gi/oswin"
	"github.com/goki/gi/units"
)

var samplefile gi.FileName = "sample.go"

func main() {
	gimain.Main(func() {
		mainrun()
	})
}

func mainrun() {
	width := 1024
	height := 768

	// gi.Layout2DTrace = true

	oswin.TheApp.SetName("textview")
	oswin.TheApp.SetAbout(`This is a demo of the TextView in the <b>GoGi</b> graphical interface system, within the <b>GoKi</b> tree framework.  See <a href="https://github.com/goki">GoKi on GitHub</a>`)

	win := gi.NewWindow2D("gogi-textview-test", "GoGi TextView Test", width, height, true) // true = pixel sizes

	vp := win.WinViewport2D()
	updt := vp.UpdateStart()

	// // style sheet
	// var css = ki.Props{
	// 	"kbd": ki.Props{
	// 		"color": "blue",
	// 	},
	// }
	// vp.CSS = css

	mfr := win.SetMainFrame()

	trow := mfr.AddNewChild(gi.KiT_Layout, "trow").(*gi.Layout)
	trow.Lay = gi.LayoutHoriz
	trow.SetStretchMaxWidth()

	title := trow.AddNewChild(gi.KiT_Label, "title").(*gi.Label)
	hdrText := `This is a <b>test</b> of the TextView`
	title.Text = hdrText
	title.SetProp("text-align", gi.AlignCenter)
	title.SetProp("vertical-align", gi.AlignTop)
	title.SetProp("font-size", "x-large")

	splt := mfr.AddNewChild(gi.KiT_SplitView, "split-view").(*gi.SplitView)
	splt.SetSplits(.5, .5)
	// these are all inherited so we can put them at the top "editor panel" level
	splt.SetProp("white-space", gi.WhiteSpacePreWrap)
	splt.SetProp("tab-size", 4)
	splt.SetProp("font-family", "Go Mono")
	splt.SetProp("line-height", 1.1)

	// generally need to put text view within its own layout for scrolling
	txly1 := splt.AddNewChild(gi.KiT_Layout, "view-layout-1").(*gi.Layout)
	txly1.SetStretchMaxWidth()
	txly1.SetStretchMaxHeight()
	txly1.SetMinPrefWidth(units.NewValue(20, units.Ch))
	txly1.SetMinPrefHeight(units.NewValue(10, units.Ch))

	txed1 := txly1.AddNewChild(giv.KiT_TextView, "textview-1").(*giv.TextView)
	txed1.Viewport = vp

	// generally need to put text view within its own layout for scrolling
	txly2 := splt.AddNewChild(gi.KiT_Layout, "view-layout-2").(*gi.Layout)
	txly2.SetStretchMaxWidth()
	txly2.SetStretchMaxHeight()
	txly2.SetMinPrefWidth(units.NewValue(20, units.Ch))
	txly2.SetMinPrefHeight(units.NewValue(10, units.Ch))

	txed2 := txly2.AddNewChild(giv.KiT_TextView, "textview-2").(*giv.TextView)
	txed2.Viewport = vp

	txbuf := giv.NewTextBuf()
	txed1.SetBuf(txbuf)
	txed2.SetBuf(txbuf)

	txbuf.Hi.Lang = "Go"
	txbuf.Hi.Style = "emacs"
	txbuf.Opts.LineNos = true
	txbuf.Open(samplefile)

	// main menu
	appnm := oswin.TheApp.Name()
	mmen := win.MainMenu
	mmen.ConfigMenus([]string{appnm, "Edit", "Window"})

	amen := win.MainMenu.ChildByName(appnm, 0).(*gi.Action)
	amen.Menu = make(gi.Menu, 0, 10)
	amen.Menu.AddAppMenu(win)

	emen := win.MainMenu.ChildByName("Edit", 1).(*gi.Action)
	emen.Menu = make(gi.Menu, 0, 10)
	emen.Menu.AddCopyCutPaste(win)

	win.OSWin.SetCloseCleanFunc(func(w oswin.Window) {
		go oswin.TheApp.Quit() // once main window is closed, quit
	})

	win.MainMenuUpdated()
	vp.UpdateEndNoSig(updt)

	win.StartEventLoop()
}

// SpellCorrectEdit uses the selected correction to edit the text
func SpellCorrectEdit(data interface{}, new string, old string) (ed spell.EditData) {
	ed = spell.CorrectText(old, new)
	return ed
}
