package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"

	"github.com/dlebee/k8s-buddy/internal/alert"
	"github.com/dlebee/k8s-buddy/internal/config"
	"github.com/dlebee/k8s-buddy/internal/monitor"
	"github.com/dlebee/k8s-buddy/internal/server"
	"github.com/dlebee/k8s-buddy/internal/ui"
)

func main() {
	headless := flag.Bool("headless", false, "no native UI; print the overview URL instead")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path (overrides settings, $KUBECONFIG and ~/.kube/config)")
	dataDir := flag.String("data-dir", "", "settings/history directory (default: OS config dir)/k8s-buddy")
	verbose := flag.Bool("v", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	// client-go is chatty about watch errors; we surface those per cluster in the UI instead.
	klog.SetLogger(logr.Discard())

	if err := run(log, *headless, *kubeconfig, *dataDir); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, headless bool, kubeconfig, dataDir string) error {
	inheritLoginShellPath(log)

	if dataDir == "" {
		d, err := config.Dir()
		if err != nil {
			return err
		}
		dataDir = d
	}
	settings, err := config.Open(dataDir)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if kubeconfig != "" {
		if err := settings.Update(func(s *config.Settings) { s.Kubeconfig = kubeconfig }); err != nil {
			return err
		}
	}

	historyPath := filepath.Join(dataDir, "history.json")
	store := alert.NewStore()
	if err := store.Load(historyPath); err != nil {
		log.Warn("could not load alert history, starting fresh", "err", err)
	}
	applySettings := func(s config.Settings) {
		store.Configure(
			[]alert.Filter{
				alert.ExcludeClusters(s.ExcludedContexts),
				alert.IgnoreNamespaces(s.IgnoredNamespaces),
				alert.Mute(s.MutedKeys),
			},
			s.HistorySize, time.Duration(s.SnoozeHours)*time.Hour, alert.Severity(s.NudgeLevel),
		)
		store.PurgeClusters(s.ExcludedContexts)
	}
	applySettings(settings.Get())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := monitor.NewManager(settings, store, log)
	srv := server.New(store, settings, mgr)
	ln, err := srv.Listen()
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()

	quit := func() {
		cancel()
		if err := store.Save(historyPath); err != nil {
			log.Warn("save history", "err", err)
		}
	}
	initial := settings.Get()
	driver := ui.New(ui.Options{
		CharacterURL:  srv.URL("character"),
		PanelURL:      srv.URL("panel"),
		OverviewURL:   srv.URL("overview"),
		ShowCharacter: initial.ShowCharacter,
		OnQuit:        quit,
		OnCharacterVisible: func(v bool) {
			_ = settings.Update(func(s *config.Settings) { s.ShowCharacter = v })
		},
		Log: log,
	}, headless)
	srv.SetActions(driver)

	var lastShow atomic.Bool
	lastShow.Store(initial.ShowCharacter)
	settings.OnChange(func(s config.Settings) {
		applySettings(s)
		if lastShow.Swap(s.ShowCharacter) != s.ShowCharacter {
			driver.SetCharacterVisible(s.ShowCharacter)
		}
	})

	go mgr.Run(ctx)
	go store.Maintain(ctx, historyPath, func(err error) { log.Warn("save history", "err", err) })
	go updateTray(ctx, store, driver)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		driver.Quit()
	}()

	log.Info("k8s-buddy started", "data", dataDir, "overview", srv.URL("overview"))
	driver.Run()
	return nil
}

func updateTray(ctx context.Context, store *alert.Store, d ui.Driver) {
	ch, unsub := store.Subscribe()
	defer unsub()
	last := ""
	refresh := func() {
		s := store.Summary()
		title := "⎈"
		if s.Nudgeable > 0 {
			title = fmt.Sprintf("⎈ %d", s.Nudgeable)
		}
		tip := fmt.Sprintf("k8s-buddy · %d open alert(s) on %d cluster(s)", s.Pending, s.Clusters)
		if title+tip != last {
			last = title + tip
			d.SetTray(title, tip)
		}
	}
	refresh()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			refresh()
		case <-t.C:
			refresh()
		}
	}
}

// GUI apps launched from Finder get a minimal PATH, which breaks kubeconfig exec plugins
// (aws, gke-gcloud-auth-plugin, kubelogin, ...). Borrow the user's login shell PATH.
func inheritLoginShellPath(log *slog.Logger) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, shell, "-l", "-c", `printf %s "$PATH"`).Output()
	extra := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if err == nil {
		extra = append(strings.Split(strings.TrimSpace(string(out)), ":"), extra...)
	} else {
		log.Debug("login shell PATH", "err", err)
	}
	seen := map[string]bool{}
	var merged []string
	for _, p := range append(filepath.SplitList(os.Getenv("PATH")), extra...) {
		if p != "" && !seen[p] {
			seen[p] = true
			merged = append(merged, p)
		}
	}
	_ = os.Setenv("PATH", strings.Join(merged, string(os.PathListSeparator)))
}
