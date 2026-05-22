//go:build linux

package gui

import (
	"os"
	"os/exec"
	"runtime"

	"github.com/getlantern/systray"
	"github.com/secureNqwer/zerolink/version"
)

func openBrowser(url string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		exec.Command("open", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

func StartTray(onQuit func()) {
	systray.Run(func() {
		systray.SetTitle(version.Name)
		systray.SetTooltip(version.Name + " - Secure Messenger")

		if _, err := os.Stat("icons/zerolink.png"); err == nil {
			icon, _ := os.ReadFile("icons/zerolink.png")
			if icon != nil {
				systray.SetIcon(icon)
			}
		}

		mOpen := systray.AddMenuItem("Open Zerolink", "Open web interface")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Quit Zerolink")

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openBrowser("http://localhost:8081")
				case <-mQuit.ClickedCh:
					systray.Quit()
					if onQuit != nil {
						onQuit()
					}
					os.Exit(0)
				}
			}
		}()
	}, func() {
		if onQuit != nil {
			onQuit()
		}
	})
}
