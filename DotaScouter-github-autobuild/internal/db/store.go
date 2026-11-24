package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"dotascouter/internal/api"
	_ "modernc.org/sqlite"
)

// Store manages the local SQLite database at %APPDATA%\DotaScouter\history.db.
type Store struct {
	db *sql.DB
}

// Open initializes the SQLite database and ensures the required schema exists.
func Open(dbPath string) (*Store, error) {
	conn, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1)

	schema := `
	CREATE TABLE IF NOT EXISTS snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		match_id INTEGER NOT NULL DEFAULT 0,
		source TEXT NOT NULL,
		radiant_win INTEGER,
		players_count INTEGER NOT NULL DEFAULT 0,
		payload_json TEXT NOT NULL,
		created_at DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS match_players (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		snapshot_id INTEGER NOT NULL,
		match_id INTEGER NOT NULL DEFAULT 0,
		account_id INTEGER NOT NULL,
		team TEXT NOT NULL,
		hero_id INTEGER NOT NULL DEFAULT 0,
		hero_name TEXT NOT NULL DEFAULT '',
		radiant_win INTEGER,
		created_at DATETIME NOT NULL,
		FOREIGN KEY (snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_match_players_account ON match_players(account_id);
	CREATE INDEX IF NOT EXISTS idx_match_players_match ON match_players(match_id);
	CREATE INDEX IF NOT EXISTS idx_snapshots_created ON snapshots(created_at DESC);
	`
	if _, err := conn.Exec(schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return &Store{db: conn}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// SaveSnapshot stores a scouted match snapshot and indexes every player for Local Encounter Memory.
func (s *Store) SaveSnapshot(snap *api.MatchScoutSnapshot) (int64, error) {
	if snap.CreatedAt.IsZero() {
		snap.CreatedAt = time.Now().UTC()
	}

	payload, err := json.Marshal(snap)
	if err != nil {
		return 0, fmt.Errorf("marshal snapshot: %w", err)
	}

	var radiantWin sql.NullInt64
	if snap.RadiantWin != nil {
		if *snap.RadiantWin {
			radiantWin = sql.NullInt64{Int64: 1, Valid: true}
		} else {
			radiantWin = sql.NullInt64{Int64: 0, Valid: true}
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	res, err := tx.Exec(
		`INSERT INTO snapshots (match_id, source, radiant_win, players_count, payload_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		snap.MatchID, snap.Source, radiantWin, len(snap.Players), string(payload), snap.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("insert snapshot: %w", err)
	}
	snapID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare(
		`INSERT INTO match_players (snapshot_id, match_id, account_id, team, hero_id, hero_name, radiant_win, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, p := range snap.Players {
		if p.AccountID <= 0 {
			continue
		}
		heroID := p.CurrentHeroID
		heroName := p.CurrentHero
		if heroName == "" && len(p.TopHeroes) > 0 {
			heroID = p.TopHeroes[0].HeroID
			heroName = p.TopHeroes[0].HeroName
		}
		if _, err = stmt.Exec(
			snapID, snap.MatchID, p.AccountID, p.Team, heroID, heroName, radiantWin, snap.CreatedAt.UTC().Format(time.RFC3339),
		); err != nil {
			return 0, fmt.Errorf("insert match_player: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	snap.ID = snapID
	return snapID, nil
}

// ListSnapshots returns recent saved match snapshots (newest first).
func (s *Store) ListSnapshots(limit int) ([]api.SnapshotListItem, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, match_id, source, players_count, created_at
		 FROM snapshots
		 ORDER BY id DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]api.SnapshotListItem, 0)
	for rows.Next() {
		var item api.SnapshotListItem
		var ts string
		if err := rows.Scan(&item.ID, &item.MatchID, &item.Source, &item.PlayersCount, &ts); err != nil {
			return nil, err
		}
		if parsed, perr := time.Parse(time.RFC3339, ts); perr == nil {
			item.CreatedAt = parsed
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetSnapshot loads a full saved snapshot by its database ID.
func (s *Store) GetSnapshot(id int64) (*api.MatchScoutSnapshot, error) {
	var payload string
	err := s.db.QueryRow(`SELECT payload_json FROM snapshots WHERE id = ?`, id).Scan(&payload)
	if err != nil {
		return nil, err
	}
	var snap api.MatchScoutSnapshot
	if err := json.Unmarshal([]byte(payload), &snap); err != nil {
		return nil, err
	}
	snap.ID = id
	return &snap, nil
}

// GetEncounterMemory checks prior snapshots for any previous encounters with accountID,
// excluding the current matchID (if > 0) so the active game isn't double-counted.
func (s *Store) GetEncounterMemory(accountID int64, excludeMatchID int64, myAccountID int64) (*api.EncounterMemory, error) {
	if accountID <= 0 || accountID == myAccountID {
		return nil, nil
	}

	rows, err := s.db.Query(
		`SELECT mp.snapshot_id, mp.match_id, mp.team, mp.hero_name, mp.radiant_win
		 FROM match_players mp
		 WHERE mp.account_id = ?
		   AND (? = 0 OR mp.match_id != ?)
		 ORDER BY mp.id DESC
		 LIMIT 25`,
		accountID, excludeMatchID, excludeMatchID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seenMatch := make(map[int64]bool)
	seenSnap := make(map[int64]bool)
	heroes := make([]string, 0, 5)
	heroSeen := make(map[string]bool)

	count := 0
	wins := 0
	losses := 0

	for rows.Next() {
		var snapID, matchID int64
		var team, heroName string
		var radiantWin sql.NullInt64
		if err := rows.Scan(&snapID, &matchID, &team, &heroName, &radiantWin); err != nil {
			continue
		}
		if matchID > 0 {
			if seenMatch[matchID] {
				continue
			}
			seenMatch[matchID] = true
		} else {
			if seenSnap[snapID] {
				continue
			}
			seenSnap[snapID] = true
		}

		count++
		if heroName != "" && !heroSeen[heroName] && len(heroes) < 4 {
			heroSeen[heroName] = true
			heroes = append(heroes, heroName)
		}

		if radiantWin.Valid {
			radWon := radiantWin.Int64 == 1
			playerWon := (team == "Radiant" && radWon) || (team == "Dire" && !radWon)
			if playerWon {
				wins++
			} else {
				losses++
			}
		}
	}

	if count == 0 {
		return nil, nil
	}

	tag := fmt.Sprintf("Met %dx", count)
	if wins+losses > 0 {
		tag = fmt.Sprintf("Met %dx · %d-%d", count, wins, losses)
	}

	return &api.EncounterMemory{
		Count:        count,
		WinsWith:     wins,
		LossesWith:   losses,
		SummaryTag:   tag,
		RecentHeroes: heroes,
	}, nil
}
