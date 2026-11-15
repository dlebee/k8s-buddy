package ui

/*
#cgo CFLAGS: -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#include <stdlib.h>
#include "ui_darwin.h"
*/
import "C"

import (
	"runtime"
	"unsafe"
)

const nativeAvailable = true

// AppKit must own the process main thread.
func init() { runtime.LockOSThread() }

var active *native

type native struct{ o Options }

func newNative(o Options) Driver {
	active = &native{o: o}
	return active
}

func cstr(s string) (*C.char, func()) {
	p := C.CString(s)
	return p, func() { C.free(unsafe.Pointer(p)) }
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func (n *native) Run() {
	ch, f1 := cstr(n.o.CharacterURL)
	defer f1()
	pa, f2 := cstr(n.o.PanelURL)
	defer f2()
	ov, f3 := cstr(n.o.OverviewURL)
	defer f3()
	C.buddy_run(ch, pa, ov, cbool(n.o.ShowCharacter))
}

func (n *native) SetTray(title, tooltip string) {
	t, f1 := cstr(title)
	defer f1()
	tt, f2 := cstr(tooltip)
	defer f2()
	C.buddy_set_tray(t, tt)
}

func (n *native) ShowOverview(tab string) {
	t, f := cstr(sanitizeTab(tab))
	defer f()
	C.buddy_show_overview(t)
}

func (n *native) TogglePanel()               { C.buddy_toggle_panel() }
func (n *native) HidePanel()                 { C.buddy_hide_panel() }
func (n *native) SetCharacterVisible(v bool) { C.buddy_set_character_visible(cbool(v)) }
func (n *native) Quit()                      { C.buddy_quit() }

//export goBuddyWillTerminate
func goBuddyWillTerminate() {
	if active != nil && active.o.OnQuit != nil {
		active.o.OnQuit()
	}
}

//export goBuddyCharacterVisible
func goBuddyCharacterVisible(v C.int) {
	if active != nil && active.o.OnCharacterVisible != nil {
		go active.o.OnCharacterVisible(v != 0)
	}
}
