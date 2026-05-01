package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	"log"
)

func main() {
	a := app.New()
	w := a.NewWindow("Test")
	
	w.Canvas().AddShortcut(&desktop.CustomShortcut{KeyName: fyne.KeyH}, func(s fyne.Shortcut) {
		log.Println("H pressed")
	})
	
	w.SetContent(widget.NewLabel("Press H"))
	// w.ShowAndRun()
}
