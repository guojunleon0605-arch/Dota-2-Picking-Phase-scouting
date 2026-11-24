package gsi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dotascouter/internal/config"
)

// Payload captures the read-only fields sent by Dota 2 Game State Integration.
type Payload struct {
	Provider *struct {
		Name    string `json:"name"`
		AppID   int    `json:"appid"`
		SteamID string `json:"steamid"`
	} `json:"provider"`
	Map *struct {
		MatchID   string `json:"matchid"`
		GameState string `json:"game_state"`
	} `json:"map"`
	Player json.RawMessage `json:"player"`
}

// StateSnapshot reports the live GSI status for the UI status bars.
type StateSnapshot struct {
	Listening    bool
	Port         int
	Connected    bool
	GameState    string
	PickingPhase bool
	MatchID      string
	LastSeen     time.Time
}

// Server listens on http://127.0.0.1:44444 for read-only Dota 2 GSI JSON POSTs.
type Server struct {
	port           int
	onPickingPhase func(gameState string, matchID int64, gsiAccountIDs []int64)
	onPostGame     func(matchID int64)
	onStatusChange func(StateSnapshot)

	mu            sync.RWMutex
	listening     bool
	gameState     string
	lastMatchID   string
	lastPickKey   string
	lastPostID    string
	lastSeen      time.Time
	httpServer    *http.Server
}

// NewServer constructs a GSI listener with separate callbacks for live Picking Phase vs Post-Game.
func NewServer(
	port int,
	onPickingPhase func(gameState string, matchID int64, gsiAccountIDs []int64),
	onPostGame func(matchID int64),
	onStatusChange func(StateSnapshot),
) *Server {
	if port <= 0 {
		port = 44444
	}
	return &Server{
		port:           port,
		onPickingPhase: onPickingPhase,
		onPostGame:     onPostGame,
		onStatusChange: onStatusChange,
	}
}

// Start binds 127.0.0.1:<port> and serves POST GSI payloads in the background.
func (s *Server) Start() error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleGSI)

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	s.mu.Lock()
	s.listening = true
	s.mu.Unlock()

	go func() {
		_ = s.httpServer.Serve(ln)
	}()

	return nil
}

// Shutdown gracefully stops the GSI HTTP listener.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// IsPickingPhaseState returns true during Dota 2 draft / hero selection / strategy time.
func IsPickingPhaseState(gs string) bool {
	switch gs {
	case "DOTA_GAMERULES_STATE_WAIT_FOR_PLAYERS_TO_LOAD",
		"DOTA_GAMERULES_STATE_CUSTOM_GAME_SETUP",
		"DOTA_GAMERULES_STATE_HERO_SELECTION",
		"DOTA_GAMERULES_STATE_STRATEGY_TIME",
		"DOTA_GAMERULES_STATE_TEAM_SHOWCASE":
		return true
	default:
		return false
	}
}

// Status returns the current GSI connection state.
func (s *Server) Status() StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	connected := !s.lastSeen.IsZero() && time.Since(s.lastSeen) < 60*time.Second
	return StateSnapshot{
		Listening:    s.listening,
		Port:         s.port,
		Connected:    connected,
		GameState:    s.gameState,
		PickingPhase: connected && IsPickingPhaseState(s.gameState),
		MatchID:      s.lastMatchID,
		LastSeen:     s.lastSeen,
	}
}

func (s *Server) handleGSI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("DotaScouter GSI endpoint listening"))
		return
	}
	defer r.Body.Close()

	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	gsiIDs := extractPlayerSteamIDs(p.Player)

	var triggerPick bool
	var pickState string
	var pickMatchID int64
	var triggerPostMatchID int64

	s.mu.Lock()
	s.lastSeen = time.Now().UTC()
	if p.Map != nil {
		if p.Map.GameState != "" {
			s.gameState = p.Map.GameState
		}
		midStr := strings.TrimSpace(p.Map.MatchID)
		var mid int64
		if midStr != "" && midStr != "0" {
			if parsed, perr := strconv.ParseInt(midStr, 10, 64); perr == nil && parsed > 0 {
				mid = parsed
				s.lastMatchID = midStr
			}
		}

		// Live Picking Phase: trigger immediate local lobby scan (OpenDota doesn't have the match until post-game)
		if IsPickingPhaseState(s.gameState) {
			pickKey := midStr + "|" + s.gameState
			if midStr != "" && s.lastPickKey != midStr {
				s.lastPickKey = midStr
				triggerPick = true
				pickState = s.gameState
				pickMatchID = mid
			} else if midStr == "" && s.lastPickKey != pickKey {
				s.lastPickKey = pickKey
				triggerPick = true
				pickState = s.gameState
			}
		}

		// Post-Game: OpenDota now indexes the finished match roster
		if s.gameState == "DOTA_GAMERULES_STATE_POST_GAME" && mid > 0 && midStr != s.lastPostID {
			s.lastPostID = midStr
			triggerPostMatchID = mid
		}
	}
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)

	if s.onStatusChange != nil {
		s.onStatusChange(s.Status())
	}
	if triggerPick && s.onPickingPhase != nil {
		go s.onPickingPhase(pickState, pickMatchID, gsiIDs)
	}
	if triggerPostMatchID > 0 && s.onPostGame != nil {
		go s.onPostGame(triggerPostMatchID)
	}
}

// extractPlayerSteamIDs parses spectator/lobby GSI payloads that include player.team2 & player.team3.
func extractPlayerSteamIDs(raw json.RawMessage) []int64 {
	if len(raw) == 0 {
		return nil
	}
	var teams map[string]map[string]struct {
		SteamID string `json:"steamid"`
	}
	if err := json.Unmarshal(raw, &teams); err != nil {
		return nil
	}

	out := make([]int64, 0, 10)
	seen := make(map[int64]bool)
	for _, teamKey := range []string{"team2", "team3"} {
		players, ok := teams[teamKey]
		if !ok {
			continue
		}
		for i := 0; i < 10; i++ {
			p, exists := players[fmt.Sprintf("player%d", i)]
			if !exists || p.SteamID == "" {
				continue
			}
			if accID, ok := config.ParseSteamAccountID(p.SteamID); ok && !seen[accID] {
				seen[accID] = true
				out = append(out, accID)
			}
		}
	}
	return out
}
