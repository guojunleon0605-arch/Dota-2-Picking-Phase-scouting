package api

import "time"

// HeroConstant represents a Dota 2 hero entry from OpenDota constants.
type HeroConstant struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	LocalizedName string `json:"localized_name"`
	ShortName     string `json:"short_name"`
	PortraitURL   string `json:"portrait_url"`
}

// TopHeroStat summarizes one of a player's Top 3 most-played heroes.
type TopHeroStat struct {
	HeroID      int     `json:"hero_id"`
	HeroName    string  `json:"hero_name"`
	PortraitURL string  `json:"portrait_url"`
	Games       int     `json:"games"`
	Wins        int     `json:"wins"`
	WinRate     float64 `json:"win_rate"`
}

// EncounterMemory summarizes prior local encounters with a player from SQLite history.
type EncounterMemory struct {
	Count        int      `json:"count"`
	WinsWith     int      `json:"wins_with"`
	LossesWith   int      `json:"losses_with"`
	SummaryTag   string   `json:"summary_tag"`
	RecentHeroes []string `json:"recent_heroes"`
}

// SteamMeta holds optional Steam Web API metadata & smurf signals.
type SteamMeta struct {
	CreatedYear int     `json:"created_year,omitempty"`
	DotaHours   float64 `json:"dota_hours,omitempty"`
	ProfileURL  string  `json:"profile_url,omitempty"`
}

// ExternalLinks provides quick links to external Dota 2 / Steam pages.
type ExternalLinks struct {
	OpenDota string `json:"opendota"`
	Stratz   string `json:"stratz"`
	Dotabuff string `json:"dotabuff"`
	Steam    string `json:"steam"`
}

// PlayerScoutResult is the full scouted row for one player (Radiant 1-5 or Dire 6-10).
type PlayerScoutResult struct {
	Slot           int              `json:"slot"`            // 1..10
	Team           string           `json:"team"`            // "Radiant" or "Dire"
	AccountID      int64            `json:"account_id"`
	SteamID64      int64            `json:"steam_id64"`
	Nickname       string           `json:"nickname"`
	AvatarURL      string           `json:"avatar_url"`
	RankTier       int              `json:"rank_tier"`
	RankLabel      string           `json:"rank_label"`
	CurrentHeroID  int              `json:"current_hero_id,omitempty"`
	CurrentHero    string           `json:"current_hero,omitempty"`
	Wins           int              `json:"wins"`
	Losses         int              `json:"losses"`
	GamesCount     int              `json:"games_count"`
	WinRate        float64          `json:"win_rate"`
	AvgKDA         float64          `json:"avg_kda"`
	TopHeroes      []TopHeroStat    `json:"top_heroes"`
	MVPCount       int              `json:"mvp_count"`
	MVPParsed      int              `json:"mvp_parsed"`
	MVPTotal       int              `json:"mvp_total"`
	MVPStatus      string           `json:"mvp_status"`     // "OK", "Add Token", "N/A", "Private Profile", "API 429 Rate Limited", etc.
	ProfileStatus  string           `json:"profile_status"` // "Public", "Archived", "Private Profile", "API 429 Rate Limited", "Error"
	ArchivedDate   string           `json:"archived_date,omitempty"`
	StatusDetail   string           `json:"status_detail,omitempty"`
	FromCache      bool             `json:"from_cache"`
	Encounter      *EncounterMemory `json:"encounter,omitempty"`
	Steam          *SteamMeta       `json:"steam,omitempty"`
	Links          ExternalLinks    `json:"links"`
	LoadedAt       time.Time        `json:"loaded_at"`
}

// MatchScoutSnapshot represents a complete 10-player (or custom N-player) scouting report.
type MatchScoutSnapshot struct {
	ID            int64               `json:"id,omitempty"`
	MatchID       int64               `json:"match_id"`
	Source        string              `json:"source"`
	RadiantWin    *bool               `json:"radiant_win,omitempty"`
	PlayersLoaded int                 `json:"players_loaded"`
	PlayersTotal  int                 `json:"players_total"`
	Diagnostics   string              `json:"diagnostics"`
	CreatedAt     time.Time           `json:"created_at"`
	Players       []PlayerScoutResult `json:"players"`
}

// SnapshotListItem is a compact summary for the History Snapshots sidebar tab.
type SnapshotListItem struct {
	ID           int64     `json:"id"`
	MatchID      int64     `json:"match_id"`
	Source       string    `json:"source"`
	PlayersCount int       `json:"players_count"`
	CreatedAt    time.Time `json:"created_at"`
}

// LiveStatus represents the current backend status shown in the top & bottom bars.
type LiveStatus struct {
	GSIListening  bool      `json:"gsi_listening"`
	GSIPort       int       `json:"gsi_port"`
	GSIConnected  bool      `json:"gsi_connected"`
	GSIGameState  string    `json:"gsi_game_state"`
	PickingPhase  bool      `json:"picking_phase"`
	GSIMatchID    string    `json:"gsi_match_id"`
	GSILastSeen   time.Time `json:"gsi_last_seen,omitempty"`
	LogWatching   bool      `json:"log_watching"`
	LogPath       string    `json:"log_path,omitempty"`
	PlayersLoaded int       `json:"players_loaded"`
	PlayersTotal  int       `json:"players_total"`
	Diagnostics   string    `json:"diagnostics"`
	IsLoading     bool      `json:"is_loading"`
}
