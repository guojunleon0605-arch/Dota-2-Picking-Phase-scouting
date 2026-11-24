//go:build windows

package platform

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// LaunchAppWindow opens the local dashboard in a frameless Edge/Chrome app window
// and returns the launched *exec.Cmd if the browser process can be awaited, or nil if
// launched via the system default browser fallback.
func LaunchAppWindow(url string) (*exec.Cmd, error) {
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("LocalAppData"), "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(os.Getenv("LocalAppData"), "Google", "Chrome", "Application", "chrome.exe"),
	}

	userDataDir := filepath.Join(os.TempDir(), "DotaScouterWebProfile")

	for _, exe := range candidates {
		if exe == "" {
			continue
		}
		if _, err := os.Stat(exe); err == nil {
			cmd := exec.Command(
				exe,
				fmt.Sprintf("--app=%s", url),
				"--window-size=1280,820",
				fmt.Sprintf("--user-data-dir=%s", userDataDir),
				"--no-first-run",
				"--no-default-browser-check",
			)
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if err := cmd.Start(); err == nil {
				return cmd, nil
			}
		}
	}

	// Fallback: try msedge on PATH
	if path, err := exec.LookPath("msedge"); err == nil {
		cmd := exec.Command(path, fmt.Sprintf("--app=%s", url), "--window-size=1280,820")
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if err := cmd.Start(); err == nil {
			return cmd, nil
		}
	}

	// Final fallback: default Windows browser
	if err := OpenExternalURL(url); err != nil {
		return nil, err
	}
	return nil, nil
}

// OpenExternalURL opens a URL in the user's default Windows browser without flashing a console window.
func OpenExternalURL(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// DetectSteamPathFromRegistry queries the Windows Registry for Steam's installation directory.
func DetectSteamPathFromRegistry() string {
	keys := []struct {
		key string
		val string
	}{
		{`HKCU\Software\Valve\Steam`, "SteamPath"},
		{`HKLM\SOFTWARE\WOW6432Node\Valve\Steam`, "InstallPath"},
		{`HKLM\SOFTWARE\Valve\Steam`, "InstallPath"},
	}

	for _, k := range keys {
		cmd := exec.Command("reg", "query", k.key, "/v", k.val)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			continue
		}
		for _, line := range strings.Split(out.String(), "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, k.val) && strings.Contains(line, "REG_SZ") {
				parts := strings.SplitN(line, "REG_SZ", 2)
				if len(parts) == 2 {
					p := strings.TrimSpace(parts[1])
					p = strings.ReplaceAll(p, "/", `\`)
					if p != "" {
						return p
					}
				}
			}
		}
	}
	return ""
}
