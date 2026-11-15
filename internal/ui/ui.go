package ui

import (
	"log/slog"
	"os/exec"
	"regexp"
	"runtime"
	"sync"
)

type Options struct {
	CharacterURL  string
	PanelURL      string
	OverviewURL   string
	ShowCharacter bool
	// OnQuit runs synchronously right before the process exits.
	OnQuit func()
	// OnCharacterVisible reports visibility changes made from native UI (tray / context menu).
	OnCharacterVisible func(bool)
	Log                *slog.Logger
}

// Driver is the platform UI. Run must be called from the main goroutine and blocks.
type Driver interface {
	Run()
	SetTray(title, tooltip string)
	ShowOverview(tab string)
	TogglePanel()
	HidePanel()
	SetCharacterVisible(bool)
	Quit()
}

var validTab = regexp.MustCompile(`^[a-z]*$`)

func sanitizeTab(tab string) string {
	if validTab.MatchString(tab) {
		return tab
	}
	return ""
}

func New(o Options, headless bool) Driver {
	if headless || !nativeAvailable {
		return newHeadless(o)
	}
	return newNative(o)
}

type headless struct {
	o    Options
	done chan struct{}
	once sync.Once
}

func newHeadless(o Options) *headless { return &headless{o: o, done: make(chan struct{})} }

func (h *headless) Run() {
	h.o.Log.Info("running headless; open the overview in your browser", "url", h.o.OverviewURL)
	<-h.done
}

func (h *headless) SetTray(string, string)   {}
func (h *headless) TogglePanel()             {}
func (h *headless) HidePanel()               {}
func (h *headless) SetCharacterVisible(bool) {}

func (h *headless) ShowOverview(tab string) {
	u := h.o.OverviewURL
	if t := sanitizeTab(tab); t != "" {
		u += "#" + t
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func (h *headless) Quit() {
	h.once.Do(func() {
		if h.o.OnQuit != nil {
			h.o.OnQuit()
		}
		close(h.done)
	})
}
