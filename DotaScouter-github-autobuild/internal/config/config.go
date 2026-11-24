package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const SteamID64Base int64 = 76561197960265728

// Settings holds all persistent user configuration saved in %APPDATA%\DotaScouter\settings.json.
type Settings struct {
	MyFriendID       string `json:"my_friend_id"`
	MyAccountID      int64  `json:"my_account_id"`
	AutoSyncEnabled  bool   `json:"auto_sync_enabled"`
	StratzToken      string `json:"stratz_token"`
	OpenDotaAPIKey   string `json:"opendota_api_key"`
	SteamAPIKey      string `json:"steam_api_key"`
	Dota2Path        string `json:"dota2_path"`
	GSIPort          int    `json:"gsi_port"`
	ShowIDColumn     bool   `json:"show_id_column"`
}

// Manager handles thread-safe loading and saving of Settings and app directories.
type Manager struct {
	mu       sync.RWMutex
	baseDir  string
	cacheDir string
	settings Settings
}

// NewManager initializes %APPDATA%\DotaScouter (or OS config dir fallback) and loads settings.
func NewManager() (*Manager, error) {
	baseDir, err := resolveBaseDir()
	if err != nil {
		return nil, err
	}
	cacheDir := filepath.Join(baseDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}

	m := &Manager{
		baseDir:  baseDir,
		cacheDir: cacheDir,
		settings: Settings{
			GSIPort:      44444,
			ShowIDColumn: true,
		},
	}

	if err := m.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	return m, nil
}

func resolveBaseDir() (string, error) {
	if appData := os.Getenv("APPDATA"); appData != "" {
		dir := filepath.Join(appData, "DotaScouter")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		return dir, nil
	}
	cfgDir, err := os.UserConfigDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return ".", nil
		}
		cfgDir = home
	}
	dir := filepath.Join(cfgDir, "DotaScouter")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) BaseDir() string  { return m.baseDir }
func (m *Manager) CacheDir() string { return m.cacheDir }
func (m *Manager) DBPath() string   { return filepath.Join(m.baseDir, "history.db") }

func (m *Manager) settingsPath() string {
	return filepath.Join(m.baseDir, "settings.json")
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.settingsPath())
	if err != nil {
		return err
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	if s.GSIPort == 0 {
		s.GSIPort = 44444
	}
	if s.MyFriendID != "" {
		if id, ok := ParseSteamAccountID(s.MyFriendID); ok {
			s.MyAccountID = id
		}
	}
	m.settings = s
	return nil
}

// Get returns a copy of the current settings.
func (m *Manager) Get() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// Update persists updated settings to disk.
func (m *Manager) Update(next Settings) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	next.MyFriendID = strings.TrimSpace(next.MyFriendID)
	next.StratzToken = strings.TrimSpace(next.StratzToken)
	next.OpenDotaAPIKey = strings.TrimSpace(next.OpenDotaAPIKey)
	next.SteamAPIKey = strings.TrimSpace(next.SteamAPIKey)
	next.Dota2Path = strings.TrimSpace(next.Dota2Path)
	if next.GSIPort == 0 {
		next.GSIPort = 44444
	}

	if next.MyFriendID != "" {
		if id, ok := ParseSteamAccountID(next.MyFriendID); ok {
			next.MyAccountID = id
			next.MyFriendID = strconv.FormatInt(id, 10)
		} else {
			return m.settings, fmt.Errorf("invalid Steam Friend ID or SteamID64: %q", next.MyFriendID)
		}
	} else {
		next.MyAccountID = 0
	}

	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return m.settings, err
	}
	if err := os.WriteFile(m.settingsPath(), data, 0o600); err != nil {
		return m.settings, err
	}
	m.settings = next
	return m.settings, nil
}

var steamID3Regex = regexp.MustCompile(`\[U:1:(\d+)\]`)

// ParseSteamAccountID converts a 32-bit Friend ID, 64-bit SteamID64, or SteamID3 ([U:1:XXXX]) into a 32-bit Dota 2 Account ID.
func ParseSteamAccountID(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	if match := steamID3Regex.FindStringSubmatch(s); len(match) == 2 {
		v, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && v > 0 {
			return v, true
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	if v > SteamID64Base {
		return v - SteamID64Base, true
	}
	return v, true
}

// AccountIDToSteamID64 converts a 32-bit Dota 2 Account ID into a 64-bit SteamID64.
func AccountIDToSteamID64(accountID int64) int64 {
	if accountID <= 0 {
		return 0
	}
	if accountID > SteamID64Base {
		return accountID
	}
	return accountID + SteamID64Base
}
