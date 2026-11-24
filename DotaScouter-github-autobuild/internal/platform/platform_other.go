//go:build !windows

package platform

import (
	"fmt"
	"os/exec"
	"runtime"
)

// LaunchAppWindow opens the local dashboard on non-Windows systems (for local dev/testing).
func LaunchAppWindow(url string) (*exec.Cmd, error) {
	if err := OpenExternalURL(url); err != nil {
		return nil, err
	}
	return nil, nil
}

// OpenExternalURL opens a URL in the default browser on macOS/Linux.
func OpenExternalURL(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		if path, err := exec.LookPath("xdg-open"); err == nil {
			return exec.Command(path, url).Start()
		}
		return fmt.Errorf("no browser launcher available on %s", runtime.GOOS)
	}
}

// DetectSteamPathFromRegistry is a no-op on non-Windows platforms.
func DetectSteamPathFromRegistry() string {
	return ""
}
