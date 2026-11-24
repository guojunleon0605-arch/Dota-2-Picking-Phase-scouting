package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dotascouter/internal/config"
)

// EncounterLookup abstracts SQLite encounter lookup to avoid circular package imports.
type EncounterLookup interface {
	GetEncounterMemory(accountID int64, excludeMatchID int64, myAccountID int64) (*EncounterMemory, error)
}

// PlayerSlotInput defines one player slot to scout.
type PlayerSlotInput struct {
	Slot          int
	Team          string
	AccountID     int64
	CurrentHeroID int
	FallbackName  string
}

// ProgressFunc reports incremental player load progress (e.g., 4/10 players loaded).
type ProgressFunc func(loaded int, total int, diag string)

// Scouter coordinates OpenDota, STRATZ, Steam Web API, and Local Encounter Memory.
type Scouter struct {
	OpenDota *OpenDotaClient
	Stratz   *StratzClient
	Steam    *SteamClient
	Cfg      *config.Manager
	Memory   EncounterLookup
}

// NewScouter constructs a Scouter orchestrator.
func NewScouter(cfg *config.Manager, mem EncounterLookup) *Scouter {
	return &Scouter{
		OpenDota: NewOpenDotaClient(cfg.CacheDir()),
		Stratz:   NewStratzClient(),
		Steam:    NewSteamClient(),
		Cfg:      cfg,
		Memory:   mem,
	}
}

// ScoutMatch fetches GET /api/matches/{match_id} and scouts all 10 players (Radiant 1-5, Dire 6-10).
func (s *Scouter) ScoutMatch(ctx context.Context, matchID int64, source string, forceRefresh bool, onProgress ProgressFunc) (*MatchScoutSnapshot, error) {
	settings := s.Cfg.Get()
	_ = s.OpenDota.EnsureHeroesLoaded(ctx, settings.OpenDotaAPIKey)

	if onProgress != nil {
		onProgress(0, 10, fmt.Sprintf("Fetching match %d roster from OpenDota...", matchID))
	}

	detail, err := s.OpenDota.GetMatchDetail(ctx, matchID, settings.OpenDotaAPIKey)
	if err != nil {
		return nil, err
	}

	radiant := make([]PlayerSlotInput, 0, 5)
	dire := make([]PlayerSlotInput, 0, 5)

	for _, p := range detail.Players {
		var accID int64
		if p.AccountID != nil {
			accID = *p.AccountID
		}
		item := PlayerSlotInput{
			AccountID:     accID,
			CurrentHeroID: p.HeroID,
			FallbackName:  p.Persona,
		}
		if p.PlayerSlot < 128 {
			item.Team = "Radiant"
			radiant = append(radiant, item)
		} else {
			item.Team = "Dire"
			dire = append(dire, item)
		}
	}

	inputs := make([]PlayerSlotInput, 0, len(radiant)+len(dire))
	for i := range radiant {
		radiant[i].Slot = i + 1
		inputs = append(inputs, radiant[i])
	}
	for i := range dire {
		dire[i].Slot = 6 + i
		inputs = append(inputs, dire[i])
	}

	snap := s.scoutInputs(ctx, matchID, source, detail.RadiantWin, inputs, forceRefresh, onProgress)
	return snap, nil
}

// ScoutAccountIDs scouts 1 to 10 explicit 32-bit Account IDs (Radiant 1-5, Dire 6-10).
func (s *Scouter) ScoutAccountIDs(ctx context.Context, accountIDs []int64, source string, forceRefresh bool, onProgress ProgressFunc) (*MatchScoutSnapshot, error) {
	if len(accountIDs) == 0 {
		return nil, fmt.Errorf("at least one Steam Account ID is required")
	}
	if len(accountIDs) > 10 {
		accountIDs = accountIDs[:10]
	}

	settings := s.Cfg.Get()
	_ = s.OpenDota.EnsureHeroesLoaded(ctx, settings.OpenDotaAPIKey)

	inputs := make([]PlayerSlotInput, 0, len(accountIDs))
	for i, id := range accountIDs {
		team := "Radiant"
		if i >= 5 {
			team = "Dire"
		}
		inputs = append(inputs, PlayerSlotInput{
			Slot:      i + 1,
			Team:      team,
			AccountID: id,
		})
	}

	snap := s.scoutInputs(ctx, 0, source, nil, inputs, forceRefresh, onProgress)
	return snap, nil
}

// SyncLatestForMyAccount pulls the latest match for the configured Friend ID and scouts all 10 players.
func (s *Scouter) SyncLatestForMyAccount(ctx context.Context, forceRefresh bool, onProgress ProgressFunc) (*MatchScoutSnapshot, error) {
	settings := s.Cfg.Get()
	if settings.MyAccountID <= 0 {
		return nil, fmt.Errorf("set your Steam Friend ID in Settings first")
	}

	if onProgress != nil {
		onProgress(0, 10, fmt.Sprintf("Finding latest match for account %d...", settings.MyAccountID))
	}

	matchID, err := s.OpenDota.GetLatestMatchID(ctx, settings.MyAccountID, settings.OpenDotaAPIKey)
	if err != nil && settings.StratzToken != "" {
		// Optional fallback via STRATZ when OpenDota is rate-limited
		if sMatchID, serr := s.Stratz.GetLatestMatchID(ctx, settings.StratzToken, settings.MyAccountID); serr == nil && sMatchID > 0 {
			matchID = sMatchID
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}

	return s.ScoutMatch(ctx, matchID, "Auto-Sync", forceRefresh, onProgress)
}

func (s *Scouter) scoutInputs(
	ctx context.Context,
	matchID int64,
	source string,
	radiantWin *bool,
	inputs []PlayerSlotInput,
	forceRefresh bool,
	onProgress ProgressFunc,
) *MatchScoutSnapshot {
	settings := s.Cfg.Get()
	total := len(inputs)
	results := make([]PlayerScoutResult, total)

	// Optional batch Steam summary lookup (single request for all players)
	steamIDs := make([]int64, 0, total)
	for _, in := range inputs {
		if in.AccountID > 0 {
			steamIDs = append(steamIDs, config.AccountIDToSteamID64(in.AccountID))
		}
	}
	steamSummaries := s.Steam.GetPlayerSummariesBatch(ctx, settings.SteamAPIKey, steamIDs)

	var loadedCount int32
	var rateLimitedCount int32
	var privateCount int32
	var archivedCount int32

	var wg sync.WaitGroup
	// Concurrency cap of 3 workers to respect OpenDota free tier
	workerSem := make(chan struct{}, 3)

	for idx, input := range inputs {
		wg.Add(1)
		go func(i int, in PlayerSlotInput) {
			defer wg.Done()
			workerSem <- struct{}{}
			defer func() { <-workerSem }()

			res := s.scoutSinglePlayer(ctx, matchID, in, settings, steamSummaries, forceRefresh)
			results[i] = res

			if res.ProfileStatus == "API 429 Rate Limited" {
				atomic.AddInt32(&rateLimitedCount, 1)
			} else if res.ProfileStatus == "Private Profile" {
				atomic.AddInt32(&privateCount, 1)
			} else if res.ProfileStatus == "Archived" {
				atomic.AddInt32(&archivedCount, 1)
			}

			done := int(atomic.AddInt32(&loadedCount, 1))
			if onProgress != nil {
				onProgress(done, total, fmt.Sprintf("Loaded %d/%d players...", done, total))
			}
		}(idx, input)
	}

	wg.Wait()

	diagParts := []string{fmt.Sprintf("%d/%d players loaded", total, total)}
	if rl := atomic.LoadInt32(&rateLimitedCount); rl > 0 {
		diagParts = append(diagParts, fmt.Sprintf("%d API 429 Rate Limited", rl))
	}
	if pr := atomic.LoadInt32(&privateCount); pr > 0 {
		diagParts = append(diagParts, fmt.Sprintf("%d Private Profile", pr))
	}
	if ar := atomic.LoadInt32(&archivedCount); ar > 0 {
		diagParts = append(diagParts, fmt.Sprintf("%d Archived", ar))
	}
	if settings.StratzToken == "" {
		diagParts = append(diagParts, "STRATZ: Add Token for MVPs")
	}

	return &MatchScoutSnapshot{
		MatchID:       matchID,
		Source:        source,
		RadiantWin:    radiantWin,
		PlayersLoaded: total,
		PlayersTotal:  total,
		Diagnostics:   strings.Join(diagParts, " · "),
		CreatedAt:     time.Now().UTC(),
		Players:       results,
	}
}

func (s *Scouter) scoutSinglePlayer(
	ctx context.Context,
	matchID int64,
	in PlayerSlotInput,
	settings config.Settings,
	steamSummaries map[int64]SteamSummaryData,
	forceRefresh bool,
) PlayerScoutResult {
	res := PlayerScoutResult{
		Slot:          in.Slot,
		Team:          in.Team,
		AccountID:     in.AccountID,
		SteamID64:     config.AccountIDToSteamID64(in.AccountID),
		CurrentHeroID: in.CurrentHeroID,
		TopHeroes:     make([]TopHeroStat, 0),
		MVPStatus:     "Add Token",
		LoadedAt:      time.Now().UTC(),
	}

	if in.CurrentHeroID > 0 {
		if h, ok := s.OpenDota.GetHero(in.CurrentHeroID); ok {
			res.CurrentHero = h.LocalizedName
		}
	}

	// Anonymous slot (account_id == 0 or 4294967295 in OpenDota match response)
	if in.AccountID <= 0 || in.AccountID == 4294967295 {
		res.AccountID = 0
		res.SteamID64 = 0
		res.Nickname = "Anonymous (Private Profile)"
		if in.FallbackName != "" {
			res.Nickname = in.FallbackName
		}
		res.ProfileStatus = "Private Profile"
		res.StatusDetail = "Expose Public Match Data is OFF"
		if settings.StratzToken != "" {
			res.MVPStatus = "Private Profile"
		}
		return res
	}

	// Step 4 of Fallback Pipeline (always populated for every valid Account ID): Quick External Links
	res.Links = ExternalLinks{
		OpenDota: fmt.Sprintf("https://www.opendota.com/players/%d", res.AccountID),
		Stratz:   fmt.Sprintf("https://stratz.com/players/%d", res.AccountID),
		Dotabuff: fmt.Sprintf("https://www.dotabuff.com/players/%d", res.AccountID),
		Steam:    fmt.Sprintf("https://steamcommunity.com/profiles/%d", res.SteamID64),
	}

	// Step 2 of Fallback Pipeline: Local Encounter Memory (SQLite)
	if s.Memory != nil {
		if enc, err := s.Memory.GetEncounterMemory(res.AccountID, matchID, settings.MyAccountID); err == nil && enc != nil {
			res.Encounter = enc
		}
	}

	// Step 3 of Fallback Pipeline: Optional Steam Web API metadata (account creation year + Dota 2 hours)
	if sum, ok := steamSummaries[res.SteamID64]; ok {
		res.Nickname = sum.PersonaName
		res.AvatarURL = sum.AvatarFull
		meta := &SteamMeta{
			CreatedYear: sum.CreatedYear,
			ProfileURL:  sum.ProfileURL,
		}
		if hrs := s.Steam.GetDota2Hours(ctx, settings.SteamAPIKey, res.SteamID64); hrs > 0 {
			meta.DotaHours = math.Round(hrs*10) / 10
		}
		res.Steam = meta
	}

	// Fetch OpenDota profile + last 20 ranked matches (+ archived fallback)
	bundle, fromCache, err := s.OpenDota.GetPlayerBundle(ctx, res.AccountID, settings.OpenDotaAPIKey, forceRefresh)
	res.FromCache = fromCache
	if err != nil {
		if errors.Is(err, ErrRateLimited) {
			res.ProfileStatus = "API 429 Rate Limited"
			res.StatusDetail = "OpenDota free tier limit hit; wait briefly or add API key"
		} else {
			res.ProfileStatus = "Error"
			res.StatusDetail = err.Error()
		}
		if res.Nickname == "" {
			if in.FallbackName != "" {
				res.Nickname = in.FallbackName
			} else {
				res.Nickname = fmt.Sprintf("Player %d", res.AccountID)
			}
		}
		if settings.StratzToken != "" {
			res.MVPStatus = "N/A"
		}
		return res
	}

	// Populate OpenDota profile metadata
	if bundle.Profile != nil {
		res.RankTier = bundle.Profile.RankTier
		res.RankLabel = FormatRankTier(bundle.Profile.RankTier)
		if bundle.Profile.Profile != nil {
			if bundle.Profile.Profile.Persona != "" {
				res.Nickname = bundle.Profile.Profile.Persona
			}
			if bundle.Profile.Profile.AvatarFull != "" {
				res.AvatarURL = bundle.Profile.Profile.AvatarFull
			}
		}
	}
	if res.Nickname == "" {
		if in.FallbackName != "" {
			res.Nickname = in.FallbackName
		} else {
			res.Nickname = fmt.Sprintf("Player %d", res.AccountID)
		}
	}

	// Evaluate recent ranked matches vs 90-day recency cutoff
	recentRanked := bundle.RankedMatches
	isStale := len(recentRanked) == 0
	if !isStale && recentRanked[0].StartTime > 0 {
		if time.Since(time.Unix(recentRanked[0].StartTime, 0)) > 90*24*time.Hour {
			isStale = true
		}
	}

	if !isStale {
		res.ProfileStatus = "Public"
		s.computeMatchStats(&res, recentRanked)
	} else {
		// Step 1 of Private Profile Fallback Pipeline: Archived Public History
		archived := bundle.ArchivedMatches
		if len(archived) == 0 && len(recentRanked) > 0 {
			archived = recentRanked
		}
		if len(archived) > 0 {
			res.ProfileStatus = "Archived"
			if archived[0].StartTime > 0 {
				res.ArchivedDate = time.Unix(archived[0].StartTime, 0).UTC().Format("2006-01-02")
				res.StatusDetail = fmt.Sprintf("Archived (Last public: %s)", res.ArchivedDate)
			} else {
				res.StatusDetail = "Archived"
			}
			s.computeMatchStats(&res, archived)
		} else {
			res.ProfileStatus = "Private Profile"
			res.StatusDetail = "No public match history exposed"
		}
	}

	// Optional STRATZ GraphQL MVP query
	if settings.StratzToken != "" {
		mvp := s.Stratz.GetPlayerMVPs(ctx, settings.StratzToken, res.AccountID)
		res.MVPCount = mvp.MVPCount
		res.MVPParsed = mvp.ParsedCount
		res.MVPTotal = mvp.TotalCount
		res.MVPStatus = mvp.Status
	} else {
		res.MVPStatus = "Add Token"
	}

	return res
}

func (s *Scouter) computeMatchStats(res *PlayerScoutResult, matches []OpenDotaPlayerMatch) {
	if len(matches) == 0 {
		return
	}
	if len(matches) > 20 {
		matches = matches[:20]
	}

	wins := 0
	losses := 0
	var totalKDA float64

	type heroAgg struct {
		games int
		wins  int
	}
	heroStats := make(map[int]*heroAgg)

	for _, m := range matches {
		isRadiant := m.PlayerSlot < 128
		won := (isRadiant && m.RadiantWin) || (!isRadiant && !m.RadiantWin)
		if won {
			wins++
		} else {
			losses++
		}

		deaths := m.Deaths
		if deaths < 1 {
			deaths = 1
		}
		totalKDA += float64(m.Kills+m.Assists) / float64(deaths)

		agg, ok := heroStats[m.HeroID]
		if !ok {
			agg = &heroAgg{}
			heroStats[m.HeroID] = agg
		}
		agg.games++
		if won {
			agg.wins++
		}
	}

	n := len(matches)
	res.Wins = wins
	res.Losses = losses
	res.GamesCount = n
	res.WinRate = math.Round((float64(wins)/float64(n))*1000) / 10
	res.AvgKDA = math.Round((totalKDA/float64(n))*10) / 10

	// Rank Top 3 Most-Played Heroes (tie-break by wins, then hero ID)
	top := make([]TopHeroStat, 0, len(heroStats))
	for hid, agg := range heroStats {
		hName := fmt.Sprintf("Hero #%d", hid)
		portrait := ""
		if h, ok := s.OpenDota.GetHero(hid); ok {
			hName = h.LocalizedName
			portrait = h.PortraitURL
		}
		wr := math.Round((float64(agg.wins)/float64(agg.games))*1000) / 10
		top = append(top, TopHeroStat{
			HeroID:      hid,
			HeroName:    hName,
			PortraitURL: portrait,
			Games:       agg.games,
			Wins:        agg.wins,
			WinRate:     wr,
		})
	}

	sort.Slice(top, func(i, j int) bool {
		if top[i].Games != top[j].Games {
			return top[i].Games > top[j].Games
		}
		if top[i].Wins != top[j].Wins {
			return top[i].Wins > top[j].Wins
		}
		return top[i].HeroID < top[j].HeroID
	})

	if len(top) > 3 {
		top = top[:3]
	}
	res.TopHeroes = top
}

// FormatRankTier converts OpenDota's 2-digit rank_tier (e.g. 64 -> Ancient IV) into a readable label.
func FormatRankTier(tier int) string {
	if tier <= 0 {
		return ""
	}
	medal := tier / 10
	star := tier % 10
	medals := map[int]string{
		1: "Herald",
		2: "Guardian",
		3: "Crusader",
		4: "Archon",
		5: "Legend",
		6: "Ancient",
		7: "Divine",
		8: "Immortal",
	}
	name, ok := medals[medal]
	if !ok {
		return ""
	}
	if medal == 8 || star == 0 {
		return name
	}
	romans := []string{"", "I", "II", "III", "IV", "V"}
	if star >= 1 && star <= 5 {
		return fmt.Sprintf("%s %s", name, romans[star])
	}
	return fmt.Sprintf("%s %d", name, star)
}
