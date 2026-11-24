package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dotascouter/internal/api"
	"dotascouter/internal/config"
	"dotascouter/internal/db"
	"dotascouter/internal/gsi"
	"dotascouter/internal/logwatch"
	"dotascouter/internal/platform"
	"dotascouter/web"
)

// AppServer exposes the local JSON API, SSE stream, and embedded frontend.
type AppServer struct {
	cfg      *config.Manager
	store    *db.Store
	scouter  *api.Scouter
	gsiSrv   *gsi.Server
	watcher  *logwatch.Watcher
	shutdown func()

	mu              sync.RWMutex
	currentSnapshot *api.MatchScoutSnapshot
	lastAutoMatchID int64
	isLoading       bool
	playersLoaded   int
	playersTotal    int
	diagnostics     string
	sseClients      map[chan sseEvent]struct{}
	sseMu           sync.Mutex
}

type sseEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// NewAppServer creates the local web UI + API server.
func NewAppServer(
	cfg *config.Manager,
	store *db.Store,
	scouter *api.Scouter,
	gsiSrv *gsi.Server,
	watcher *logwatch.Watcher,
	onShutdown func(),
) *AppServer {
	return &AppServer{
		cfg:         cfg,
		store:       store,
		scouter:     scouter,
		gsiSrv:      gsiSrv,
		watcher:     watcher,
		shutdown:    onShutdown,
		diagnostics: "Ready — enter Picking Phase in Dota 2, click Scan Live Draft, or query a Match/Steam ID.",
		sseClients:  make(map[chan sseEvent]struct{}),
	}
}

// Handler returns the http.Handler serving the embedded UI and /api/* routes.
func (a *AppServer) Handler() (http.Handler, error) {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/state", a.handleGetState)
	mux.HandleFunc("/api/events", a.handleSSE)
	mux.HandleFunc("/api/settings", a.handleSettings)
	mux.HandleFunc("/api/settings/test-stratz", a.handleTestStratz)
	mux.HandleFunc("/api/settings/install-gsi", a.handleInstallGSI)
	mux.HandleFunc("/api/settings/clear-cache", a.handleClearCache)
	mux.HandleFunc("/api/scout/sync-latest", a.handleSyncLatest)
	mux.HandleFunc("/api/scout/scan-lobby", a.handleScanLobby)
	mux.HandleFunc("/api/scout/match", a.handleScoutMatch)
	mux.HandleFunc("/api/scout/players", a.handleScoutPlayers)
	mux.HandleFunc("/api/scout/refresh", a.handleRefreshCurrent)
	mux.HandleFunc("/api/history", a.handleListHistory)
	mux.HandleFunc("/api/history/", a.handleGetHistoryItem)
	mux.HandleFunc("/api/open-url", a.handleOpenURL)
	mux.HandleFunc("/api/shutdown", a.handleShutdown)

	staticFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	return mux, nil
}

// HandlePickingPhaseTrigger runs automatically the moment Dota 2 GSI enters Hero Selection / Strategy Time.
func (a *AppServer) HandlePickingPhaseTrigger(ctx context.Context, gameState string, matchID int64, gsiAccountIDs []int64) {
	a.setDiagnostics(fmt.Sprintf("Picking Phase detected (%s) — scanning live lobby roster...", gameState))

	if len(gsiAccountIDs) >= 2 {
		_, _ = a.TriggerPlayersScout(ctx, gsiAccountIDs, "Live Picking Phase (GSI)", false)
		return
	}

	// Retry scanning server_log.txt / console.log briefly in case Dota 2 is still flushing the lobby line
	for attempt := 0; attempt < 6; attempt++ {
		if ids, src := a.watcher.ScanLatestLobby(); len(ids) >= 2 {
			_, _ = a.TriggerPlayersScout(ctx, ids, src, false)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(700 * time.Millisecond):
		}
	}

	a.setDiagnostics("Picking Phase active — type 'status' in Dota 2 console (with -condebug) or click Scan Live Draft.")
}

// RunAutoSyncLoop checks every 30 seconds for a new match when AutoSyncEnabled is toggled on.
func (a *AppServer) RunAutoSyncLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			settings := a.cfg.Get()
			if !settings.AutoSyncEnabled || settings.MyAccountID <= 0 {
				continue
			}
			a.mu.RLock()
			busy := a.isLoading
			lastID := a.lastAutoMatchID
			a.mu.RUnlock()
			if busy {
				continue
			}

			checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			matchID, err := a.scouter.OpenDota.GetLatestMatchID(checkCtx, settings.MyAccountID, settings.OpenDotaAPIKey)
			cancel()
			if err != nil || matchID <= 0 || matchID == lastID {
				continue
			}

			a.mu.Lock()
			a.lastAutoMatchID = matchID
			a.mu.Unlock()
			go func(mid int64) {
				_, _ = a.TriggerMatchScout(context.Background(), mid, "Auto-Sync", false)
			}(matchID)
		}
	}
}

// NotifyGSIStatus broadcasts updated GSI connection status to connected UI clients.
func (a *AppServer) NotifyGSIStatus(_ gsi.StateSnapshot) {
	a.broadcast("status", a.buildLiveStatus())
}

// TriggerMatchScout runs a 10-player match scout, saves the snapshot to SQLite, and pushes updates via SSE.
func (a *AppServer) TriggerMatchScout(ctx context.Context, matchID int64, source string, forceRefresh bool) (*api.MatchScoutSnapshot, error) {
	a.setLoading(true, 0, 10, fmt.Sprintf("Scouting match %d (%s)...", matchID, source))
	defer a.setLoading(false, -1, -1, "")

	snap, err := a.scouter.ScoutMatch(ctx, matchID, source, forceRefresh, func(loaded, total int, diag string) {
		a.setLoading(true, loaded, total, diag)
	})
	if err != nil {
		a.setDiagnostics(err.Error())
		return nil, err
	}

	_, _ = a.store.SaveSnapshot(snap)
	a.mu.Lock()
	a.currentSnapshot = snap
	a.lastAutoMatchID = snap.MatchID
	a.playersLoaded = snap.PlayersLoaded
	a.playersTotal = snap.PlayersTotal
	a.diagnostics = snap.Diagnostics
	a.mu.Unlock()

	a.broadcast("snapshot", snap)
	a.broadcast("status", a.buildLiveStatus())
	return snap, nil
}

// TriggerPlayersScout runs a 1..10 player scout, saves the snapshot to SQLite, and pushes updates via SSE.
func (a *AppServer) TriggerPlayersScout(ctx context.Context, accountIDs []int64, source string, forceRefresh bool) (*api.MatchScoutSnapshot, error) {
	total := len(accountIDs)
	if total > 10 {
		total = 10
	}
	a.setLoading(true, 0, total, fmt.Sprintf("Scouting %d players (%s)...", total, source))
	defer a.setLoading(false, -1, -1, "")

	snap, err := a.scouter.ScoutAccountIDs(ctx, accountIDs, source, forceRefresh, func(loaded, tot int, diag string) {
		a.setLoading(true, loaded, tot, diag)
	})
	if err != nil {
		a.setDiagnostics(err.Error())
		return nil, err
	}

	_, _ = a.store.SaveSnapshot(snap)
	a.mu.Lock()
	a.currentSnapshot = snap
	a.playersLoaded = snap.PlayersLoaded
	a.playersTotal = snap.PlayersTotal
	a.diagnostics = snap.Diagnostics
	a.mu.Unlock()

	a.broadcast("snapshot", snap)
	a.broadcast("status", a.buildLiveStatus())
	return snap, nil
}

func (a *AppServer) setLoading(loading bool, loaded, total int, diag string) {
	a.mu.Lock()
	a.isLoading = loading
	if loaded >= 0 {
		a.playersLoaded = loaded
	}
	if total >= 0 {
		a.playersTotal = total
	}
	if diag != "" {
		a.diagnostics = diag
	}
	a.mu.Unlock()
	a.broadcast("status", a.buildLiveStatus())
}

func (a *AppServer) setDiagnostics(diag string) {
	a.mu.Lock()
	a.diagnostics = diag
	a.mu.Unlock()
	a.broadcast("status", a.buildLiveStatus())
}

func (a *AppServer) buildLiveStatus() api.LiveStatus {
	gsiState := a.gsiSrv.Status()
	logActive, logPath := a.watcher.Status()

	a.mu.RLock()
	defer a.mu.RUnlock()

	return api.LiveStatus{
		GSIListening:  gsiState.Listening,
		GSIPort:       gsiState.Port,
		GSIConnected:  gsiState.Connected,
		GSIGameState:  gsiState.GameState,
		PickingPhase:  gsiState.PickingPhase,
		GSIMatchID:    gsiState.MatchID,
		GSILastSeen:   gsiState.LastSeen,
		LogWatching:   logActive,
		LogPath:       logPath,
		PlayersLoaded: a.playersLoaded,
		PlayersTotal:  a.playersTotal,
		Diagnostics:   a.diagnostics,
		IsLoading:     a.isLoading,
	}
}

func (a *AppServer) broadcast(eventType string, data interface{}) {
	evt := sseEvent{Type: eventType, Data: data}
	a.sseMu.Lock()
	defer a.sseMu.Unlock()
	for ch := range a.sseClients {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (a *AppServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan sseEvent, 16)
	a.sseMu.Lock()
	a.sseClients[ch] = struct{}{}
	a.sseMu.Unlock()

	defer func() {
		a.sseMu.Lock()
		delete(a.sseClients, ch)
		a.sseMu.Unlock()
	}()

	writeSSE(w, flusher, sseEvent{Type: "status", Data: a.buildLiveStatus()})

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-ch:
			writeSSE(w, flusher, evt)
		case <-keepAlive.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, evt sseEvent) {
	b, err := json.Marshal(evt)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}

func (a *AppServer) handleGetState(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	snap := a.currentSnapshot
	a.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"settings":           a.cfg.Get(),
		"status":             a.buildLiveStatus(),
		"snapshot":           snap,
		"detected_dota_path": gsi.DetectDota2Dir(a.cfg.Get().Dota2Path),
	})
}

func (a *AppServer) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.cfg.Get())
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var next config.Settings
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	saved, err := a.cfg.Update(next)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if dotaRoot := gsi.DetectDota2Dir(saved.Dota2Path); dotaRoot != "" {
		a.watcher.SetDotaRoot(dotaRoot)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"settings": saved,
		"message":  "Settings saved.",
	})
}

func (a *AppServer) handleTestStratz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	token := strings.TrimSpace(req.Token)
	if token == "" {
		token = a.cfg.Get().StratzToken
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	msg, err := a.scouter.Stratz.TestToken(ctx, token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "message": msg})
}

func (a *AppServer) handleInstallGSI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Dota2Path string `json:"dota2_path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	settings := a.cfg.Get()
	target := strings.TrimSpace(req.Dota2Path)
	if target == "" {
		target = settings.Dota2Path
	}

	cfgPath, err := gsi.InstallGSIConfig(target, settings.GSIPort)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":       true,
		"path":     cfgPath,
		"reminder": "Add '-gamestateintegration -condebug' to Dota 2 Steam Launch Options (Steam → Dota 2 → Properties → General → Launch Options), then restart Dota 2 for live Picking Phase detection.",
	})
}

func (a *AppServer) handleClearCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	removed, err := a.scouter.OpenDota.ClearPlayerCache()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":      true,
		"removed": removed,
		"message": fmt.Sprintf("Cleared %d cached player files.", removed),
	})
}

func (a *AppServer) handleScanLobby(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ids, src := a.watcher.ScanLatestLobby()
	if len(ids) < 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "No live lobby roster found in server_log.txt or console.log yet. Ensure Dota 2 folder is set in Settings, add -condebug to Steam Launch Options, and type 'status' in Dota 2 console during Picking Phase.",
		})
		return
	}

	snap, err := a.TriggerPlayersScout(r.Context(), ids, src, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *AppServer) handleSyncLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	force := r.URL.Query().Get("force") == "1"

	a.setLoading(true, 0, 10, "Finding latest match for your Steam Friend ID...")
	defer a.setLoading(false, -1, -1, "")

	snap, err := a.scouter.SyncLatestForMyAccount(r.Context(), force, func(loaded, total int, diag string) {
		a.setLoading(true, loaded, total, diag)
	})
	if err != nil {
		a.setDiagnostics(err.Error())
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	_, _ = a.store.SaveSnapshot(snap)
	a.mu.Lock()
	a.currentSnapshot = snap
	a.lastAutoMatchID = snap.MatchID
	a.playersLoaded = snap.PlayersLoaded
	a.playersTotal = snap.PlayersTotal
	a.diagnostics = snap.Diagnostics
	a.mu.Unlock()

	a.broadcast("snapshot", snap)
	a.broadcast("status", a.buildLiveStatus())
	writeJSON(w, http.StatusOK, snap)
}

func (a *AppServer) handleScoutMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		MatchID      string `json:"match_id"`
		ForceRefresh bool   `json:"force_refresh"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	mid, err := strconv.ParseInt(strings.TrimSpace(req.MatchID), 10, 64)
	if err != nil || mid <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enter a valid numeric Dota 2 Match ID"})
		return
	}

	snap, err := a.TriggerMatchScout(r.Context(), mid, "Manual Match", req.ForceRefresh)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *AppServer) handleScoutPlayers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RawIDs       string `json:"raw_ids"`
		ForceRefresh bool   `json:"force_refresh"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}

	tokens := strings.FieldsFunc(req.RawIDs, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';' || r == ' ' || r == '\t'
	})
	ids := make([]int64, 0, 10)
	seen := make(map[int64]bool)
	for _, tok := range tokens {
		if id, ok := config.ParseSteamAccountID(tok); ok && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
			if len(ids) == 10 {
				break
			}
		}
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no valid Steam Account IDs or Friend IDs found"})
		return
	}

	snap, err := a.TriggerPlayersScout(r.Context(), ids, "Manual IDs", req.ForceRefresh)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *AppServer) handleRefreshCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.mu.RLock()
	curr := a.currentSnapshot
	a.mu.RUnlock()

	if curr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no match loaded yet to refresh"})
		return
	}

	if curr.MatchID > 0 {
		snap, err := a.TriggerMatchScout(r.Context(), curr.MatchID, curr.Source, true)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, snap)
		return
	}

	ids := make([]int64, 0, len(curr.Players))
	for _, p := range curr.Players {
		if p.AccountID > 0 {
			ids = append(ids, p.AccountID)
		}
	}
	snap, err := a.TriggerPlayersScout(r.Context(), ids, curr.Source, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *AppServer) handleListHistory(w http.ResponseWriter, r *http.Request) {
	items, err := a.store.ListSnapshots(50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *AppServer) handleGetHistoryItem(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/history/")
	id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid snapshot ID"})
		return
	}
	snap, err := a.store.GetSnapshot(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "snapshot not found"})
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (a *AppServer) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	u := strings.TrimSpace(req.URL)
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only http(s) URLs allowed"})
		return
	}
	if err := platform.OpenExternalURL(u); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *AppServer) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if a.shutdown != nil {
		go func() {
			time.Sleep(150 * time.Millisecond)
			a.shutdown()
		}()
	}
}

func writeJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}
