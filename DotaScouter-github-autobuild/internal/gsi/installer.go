package gsi

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dotascouter/internal/platform"
)

var vdfPathRegex = regexp.MustCompile(`"path"\s+"([^"]+)"`)

// DetectDota2Dir locates the Dota 2 installation folder using:
// 1) User-supplied override path
// 2) Windows Registry SteamPath + libraryfolders.vdf
// 3) Common default Steam paths on Windows (C:\, D:\, E:\)
func DetectDota2Dir(configuredPath string) string {
	if configuredPath != "" {
		if normalized := normalizeDotaRoot(configuredPath); normalized != "" {
			return normalized
		}
	}

	steamRoots := make([]string, 0, 6)
	if regPath := platform.DetectSteamPathFromRegistry(); regPath != "" {
		steamRoots = append(steamRoots, regPath)
	}
	steamRoots = append(steamRoots,
		`C:\Program Files (x86)\Steam`,
		`C:\Program Files\Steam`,
		`D:\Steam`,
		`D:\SteamLibrary`,
		`E:\SteamLibrary`,
	)

	seen := make(map[string]bool)
	for _, root := range steamRoots {
		root = strings.TrimSpace(root)
		if root == "" || seen[strings.ToLower(root)] {
			continue
		}
		seen[strings.ToLower(root)] = true

		// Check direct steamapps\common\dota 2 beta
		direct := filepath.Join(root, "steamapps", "common", "dota 2 beta")
		if isValidDotaRoot(direct) {
			return direct
		}

		// Parse steamapps\libraryfolders.vdf for additional Steam libraries
		vdfPath := filepath.Join(root, "steamapps", "libraryfolders.vdf")
		if raw, err := os.ReadFile(vdfPath); err == nil {
			matches := vdfPathRegex.FindAllStringSubmatch(string(raw), -1)
			for _, m := range matches {
				if len(m) < 2 {
					continue
				}
				libPath := strings.ReplaceAll(m[1], `\\`, `\`)
				candidate := filepath.Join(libPath, "steamapps", "common", "dota 2 beta")
				if isValidDotaRoot(candidate) {
					return candidate
				}
			}
		}
	}

	return ""
}

func normalizeDotaRoot(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if isValidDotaRoot(p) {
		return p
	}
	// If user selected "...\dota 2 beta\game\dota", walk up to "dota 2 beta"
	lower := strings.ToLower(filepath.ToSlash(p))
	if idx := strings.Index(lower, "/game/dota"); idx > 0 {
		root := p[:idx]
		if isValidDotaRoot(root) {
			return root
		}
	}
	return ""
}

func isValidDotaRoot(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "game", "dota"))
	return err == nil && info.IsDir()
}

// InstallGSIConfig writes gamestate_integration_scouter.cfg into game\dota\cfg\gamestate_integration\.
func InstallGSIConfig(dotaRoot string, port int) (string, error) {
	if port <= 0 {
		port = 44444
	}
	root := DetectDota2Dir(dotaRoot)
	if root == "" {
		return "", fmt.Errorf("could not auto-detect Dota 2 installation folder; please set it manually in Settings (e.g. C:\\Program Files (x86)\\Steam\\steamapps\\common\\dota 2 beta)")
	}

	gsiDir := filepath.Join(root, "game", "dota", "cfg", "gamestate_integration")
	if err := os.MkdirAll(gsiDir, 0o755); err != nil {
		return "", fmt.Errorf("create GSI directory: %w", err)
	}

	cfgPath := filepath.Join(gsiDir, "gamestate_integration_scouter.cfg")
	content := fmt.Sprintf(`"DotaScouter GSI Configuration"
{
	"uri"           "http://127.0.0.1:%d"
	"timeout"       "5.0"
	"buffer"        "0.1"
	"throttle"      "0.2"
	"heartbeat"     "15.0"
	"data"
	{
		"provider"      "1"
		"map"           "1"
		"player"        "1"
		"hero"          "0"
		"abilities"     "0"
		"items"         "0"
	}
}
`, port)

	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write GSI config: %w", err)
	}
	return cfgPath, nil
}
