//go:build windows

package main

import (
	"flag"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"fyne.io/systray"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/policy"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/state"
)

func main() {
	base := flag.String("api", "http://127.0.0.1:8481", "Benchwarmer management API base URL")
	tokenFile := flag.String("agent-token-file", filepath.Join(os.Getenv("ProgramData"), "Benchwarmer", "secrets", "agent.token"), "agent token file")
	interval := flag.Duration("report-interval", 2*time.Second, "session report interval")
	flag.Parse()
	c := newClient(*base, *tokenFile)
	go agentLoop(c, *interval)
	systray.Run(func() { onReady(c, *base) }, func() {})
}

// agentLoop reports session facts until the process exits.
func agentLoop(c *client, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for range t.C {
		ss, err := signals.ReadSessionSignals()
		if err != nil {
			continue
		}
		_ = c.report(api.AgentReport{
			SessionID: ss.SessionID, ForegroundPID: ss.ForegroundPID, ForegroundName: ss.ForegroundName,
			ForegroundPath: ss.ForegroundPath, Fullscreen: ss.Fullscreen, Notification: ss.NotificationState,
			IdleSeconds: ss.IdleSeconds, Locked: signals.WorkstationLocked(),
		})
	}
}

type durationItem struct {
	item     *systray.MenuItem
	duration string
}

func onReady(c *client, base string) {
	systray.SetTitle("Benchwarmer")
	systray.SetIcon(iconFor(state.Unavailable, false))
	systray.SetTooltip("Benchwarmer: connecting")

	status := systray.AddMenuItem("Connecting...", "")
	status.Disable()
	systray.AddSeparator()
	auto := systray.AddMenuItemCheckbox("Auto", "Normal policy", false)
	pause := systray.AddMenuItemCheckbox("Pause AI", "Unload the model and keep the GPU free", false)
	prio := systray.AddMenuItemCheckbox("AI Priority", "Keep the model available unless a game or real contention needs the GPU", false)
	var pauseItems, prioItems []durationItem
	for _, d := range []struct{ label, dur string }{
		{"30 minutes", "30m"}, {"1 hour", "1h"}, {"2 hours", "2h"}, {"4 hours", "4h"},
		{"Until tomorrow", "until_tomorrow"}, {"Until reboot", "until_reboot"}, {"Until I turn it off", ""},
	} {
		pauseItems = append(pauseItems, durationItem{pause.AddSubMenuItem(d.label, ""), d.dur})
	}
	for _, d := range []struct{ label, dur string }{
		{"30 minutes", "30m"}, {"1 hour", "1h"}, {"2 hours", "2h"}, {"Until tomorrow", "until_tomorrow"},
	} {
		prioItems = append(prioItems, durationItem{prio.AddSubMenuItem(d.label, ""), d.dur})
	}
	systray.AddSeparator()
	dash := systray.AddMenuItem("Open dashboard", "")
	signin := systray.AddMenuItem("Sign in to change settings...", "Opens the dashboard signed in as administrator (asks for permission)")
	quit := systray.AddMenuItem("Close tray", "The service keeps running")

	refresh := func() {
		s, err := c.status()
		systray.SetIcon(iconFor(s.Condition, err == nil))
		systray.SetTooltip(tooltip(s, err))
		if err != nil {
			status.SetTitle("Service not reachable")
			return
		}
		status.SetTitle(string(s.Condition) + ": " + s.Summary)
		setCheck(auto, s.Mode.Mode == policy.ModeAuto)
		setCheck(pause, s.Mode.Mode == policy.ModePause)
		setCheck(prio, s.Mode.Mode == policy.ModeAIPriority)
	}
	go func() {
		refresh()
		t := time.NewTicker(3 * time.Second)
		for range t.C {
			refresh()
		}
	}()
	go func() {
		for range auto.ClickedCh {
			logErr(c.setMode(policy.ModeAuto, ""))
			refresh()
		}
	}()
	for _, it := range pauseItems {
		go func(it durationItem) {
			for range it.item.ClickedCh {
				logErr(c.setMode(policy.ModePause, it.duration))
				refresh()
			}
		}(it)
	}
	for _, it := range prioItems {
		go func(it durationItem) {
			for range it.item.ClickedCh {
				logErr(c.setMode(policy.ModeAIPriority, it.duration))
				refresh()
			}
		}(it)
	}
	go func() {
		for range dash.ClickedCh {
			_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", base+"/").Start()
		}
	}()
	go func() {
		for range signin.ClickedCh {
			logErr(signIn(base))
		}
	}()
	go func() {
		<-quit.ClickedCh
		systray.Quit()
	}()
}

func setCheck(m *systray.MenuItem, on bool) {
	if on {
		m.Check()
	} else {
		m.Uncheck()
	}
}

func logErr(err error) {
	if err != nil {
		log.Printf("bwtray: %v", err)
	}
}
