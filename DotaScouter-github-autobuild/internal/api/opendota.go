package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openDotaBaseURL = "https://api.opendota.com/api"
	heroPortraitCDN = "https://cdn.cloudflare.steamstatic.com/apps/dota2/images/dota_react/heroes/%s.png"
	playerCacheTTL  = 15 * time.Minute
	heroCacheTTL    = 24 * time.Hour
)

// ErrRateLimited signals HTTP 429 from OpenDota.
var ErrRateLimited = errors.New("API 429 Rate Limited")

// OpenDotaClient executes rate-limited, cached requests against OpenDota's public REST API.
type OpenDotaClient struct {
	httpClient *http.Client
	cacheDir   string
	sem        chan struct{}

	mu     sync.RWMutex
	heroes map[int]HeroConstant
}

// NewOpenDotaClient creates an OpenDota client with max 3 concurrent requests.
func NewOpenDotaClient(cacheDir string) *OpenDotaClient {
	return &OpenDotaClient{
		httpClient: &http.Client{Timeout: 18 * time.Second},
		cacheDir:   cacheDir,
		sem:        make(chan struct{}, 3),
		heroes:     make(map[int]HeroConstant),
	}
}

// OpenDotaPlayerResponse matches GET /api/players/{account_id}.
type OpenDotaPlayerResponse struct {
	RankTier int `json:"rank_tier"`
	Profile  *struct {
		AccountID  int64  `json:"account_id"`
		Persona    string `json:"personaname"`
		AvatarFull string `json:"avatarfull"`
		ProfileURL string `json:"profileurl"`
	} `json:"profile"`
}

// OpenDotaPlayerMatch matches elements of GET /api/players/{account_id}/matches.
type OpenDotaPlayerMatch struct {
	MatchID    int64 `json:"match_id"`
	PlayerSlot int   `json:"player_slot"`
	RadiantWin bool  `json:"radiant_win"`
	HeroID     int   `json:"hero_id"`
	StartTime  int64 `json:"start_time"`
	LobbyType  int   `json:"lobby_type"`
	Kills      int   `json:"kills"`
	Deaths     int   `json:"deaths"`
	Assists    int   `json:"assists"`
}

// OpenDotaMatchDetail matches GET /api/matches/{match_id}.
type OpenDotaMatchDetail struct {
	MatchID    int64 `json:"match_id"`
	RadiantWin *bool `json:"radiant_win"`
	StartTime  int64 `json:"start_time"`
	Players    []struct {
		AccountID  *int64 `json:"account_id"`
		PlayerSlot int    `json:"player_slot"`
		HeroID     int    `json:"hero_id"`
		Persona    string `json:"personaname"`
	} `json:"players"`
}

type cachedPlayerPayload struct {
	FetchedAt       time.Time               `json:"fetched_at"`
	Profile         *OpenDotaPlayerResponse `json:"profile"`
	RankedMatches   []OpenDotaPlayerMatch   `json:"ranked_matches"`
	ArchivedMatches []OpenDotaPlayerMatch   `json:"archived_matches"`
}

// EnsureHeroesLoaded fetches and caches https://api.opendota.com/api/constants/heroes on startup.
func (c *OpenDotaClient) EnsureHeroesLoaded(ctx context.Context, apiKey string) error {
	c.mu.RLock()
	if len(c.heroes) > 0 {
		c.mu.RUnlock()
		return nil
	}
	c.mu.RUnlock()

	cachePath := filepath.Join(c.cacheDir, "heroes.json")
	if info, err := os.Stat(cachePath); err == nil && time.Since(info.ModTime()) < heroCacheTTL {
		if raw, rerr := os.ReadFile(cachePath); rerr == nil {
			if m, perr := parseHeroConstants(raw); perr == nil && len(m) > 0 {
				c.mu.Lock()
				c.heroes = m
				c.mu.Unlock()
				return nil
			}
		}
	}

	raw, err := c.doGET(ctx, "/constants/heroes", nil, apiKey)
	if err != nil {
		// Fall back to stale cache if present
		if stale, rerr := os.ReadFile(cachePath); rerr == nil {
			if m, perr := parseHeroConstants(stale); perr == nil && len(m) > 0 {
				c.mu.Lock()
				c.heroes = m
				c.mu.Unlock()
				return nil
			}
		}
		return fmt.Errorf("fetch hero constants: %w", err)
	}

	m, err := parseHeroConstants(raw)
	if err != nil {
		return err
	}
	_ = os.WriteFile(cachePath, raw, 0o644)

	c.mu.Lock()
	c.heroes = m
	c.mu.Unlock()
	return nil
}

func parseHeroConstants(raw []byte) (map[int]HeroConstant, error) {
	var rawMap map[string]struct {
		ID            int    `json:"id"`
		Name          string `json:"name"`
		LocalizedName string `json:"localized_name"`
	}
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		return nil, err
	}
	out := make(map[int]HeroConstant, len(rawMap))
	for _, h := range rawMap {
		short := strings.TrimPrefix(h.Name, "npc_dota_hero_")
		out[h.ID] = HeroConstant{
			ID:            h.ID,
			Name:          h.Name,
			LocalizedName: h.LocalizedName,
			ShortName:     short,
			PortraitURL:   fmt.Sprintf(heroPortraitCDN, short),
		}
	}
	return out, nil
}

// GetHero returns cached hero metadata by hero_id.
func (c *OpenDotaClient) GetHero(heroID int) (HeroConstant, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.heroes[heroID]
	return h, ok
}

// GetLatestMatchID returns the most recent public match ID for an account.
func (c *OpenDotaClient) GetLatestMatchID(ctx context.Context, accountID int64, apiKey string) (int64, error) {
	q := url.Values{}
	q.Set("limit", "1")
	raw, err := c.doGET(ctx, fmt.Sprintf("/players/%d/matches", accountID), q, apiKey)
	if err != nil {
		return 0, err
	}
	var matches []OpenDotaPlayerMatch
	if err := json.Unmarshal(raw, &matches); err != nil {
		return 0, err
	}
	if len(matches) == 0 {
		return 0, fmt.Errorf("no public matches found for account %d (profile may be private)", accountID)
	}
	return matches[0].MatchID, nil
}

// GetMatchDetail fetches GET /api/matches/{match_id}.
func (c *OpenDotaClient) GetMatchDetail(ctx context.Context, matchID int64, apiKey string) (*OpenDotaMatchDetail, error) {
	raw, err := c.doGET(ctx, fmt.Sprintf("/matches/%d", matchID), nil, apiKey)
	if err != nil {
		return nil, err
	}
	var detail OpenDotaMatchDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		return nil, err
	}
	if detail.MatchID == 0 || len(detail.Players) == 0 {
		return nil, fmt.Errorf("match %d not found or has no public player slots", matchID)
	}
	return &detail, nil
}

// GetPlayerBundle fetches profile + last 20 ranked matches (+ archived matches if needed),
// using a 15-minute disk cache unless forceRefresh is true.
func (c *OpenDotaClient) GetPlayerBundle(ctx context.Context, accountID int64, apiKey string, forceRefresh bool) (*cachedPlayerPayload, bool, error) {
	cachePath := filepath.Join(c.cacheDir, fmt.Sprintf("player_%d.json", accountID))
	if !forceRefresh {
		if raw, err := os.ReadFile(cachePath); err == nil {
			var cached cachedPlayerPayload
			if json.Unmarshal(raw, &cached) == nil && time.Since(cached.FetchedAt) < playerCacheTTL {
				return &cached, true, nil
			}
		}
	}

	profileRaw, err := c.doGET(ctx, fmt.Sprintf("/players/%d", accountID), nil, apiKey)
	if err != nil {
		return nil, false, err
	}
	var profile OpenDotaPlayerResponse
	if err := json.Unmarshal(profileRaw, &profile); err != nil {
		return nil, false, err
	}

	rankedQ := url.Values{}
	rankedQ.Set("lobby_type", "7")
	rankedQ.Set("limit", "20")
	rankedRaw, err := c.doGET(ctx, fmt.Sprintf("/players/%d/matches", accountID), rankedQ, apiKey)
	if err != nil {
		return nil, false, err
	}
	var ranked []OpenDotaPlayerMatch
	if err := json.Unmarshal(rankedRaw, &ranked); err != nil {
		return nil, false, err
	}

	bundle := &cachedPlayerPayload{
		FetchedAt:     time.Now().UTC(),
		Profile:       &profile,
		RankedMatches: ranked,
	}

	// Step 1 of Private Profile Fallback Pipeline:
	// If 0 recent ranked matches or most recent ranked match is older than 90 days,
	// query OpenDota without lobby_type/recency cutoff for archived historical public matches.
	needsArchive := len(ranked) == 0
	if !needsArchive && ranked[0].StartTime > 0 {
		if time.Since(time.Unix(ranked[0].StartTime, 0)) > 90*24*time.Hour {
			needsArchive = true
		}
	}
	if needsArchive {
		archQ := url.Values{}
		archQ.Set("limit", "20")
		if archRaw, aerr := c.doGET(ctx, fmt.Sprintf("/players/%d/matches", accountID), archQ, apiKey); aerr == nil {
			var archived []OpenDotaPlayerMatch
			if json.Unmarshal(archRaw, &archived) == nil {
				bundle.ArchivedMatches = archived
			}
		}
	}

	if encoded, merr := json.Marshal(bundle); merr == nil {
		_ = os.WriteFile(cachePath, encoded, 0o644)
	}

	return bundle, false, nil
}

// ClearPlayerCache deletes cached player JSON files (keeping heroes.json intact).
func (c *OpenDotaClient) ClearPlayerCache() (int, error) {
	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "player_") && strings.HasSuffix(e.Name(), ".json") {
			if err := os.Remove(filepath.Join(c.cacheDir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

func (c *OpenDotaClient) doGET(ctx context.Context, path string, query url.Values, apiKey string) ([]byte, error) {
	if query == nil {
		query = url.Values{}
	}
	if apiKey != "" {
		query.Set("api_key", apiKey)
	}
	fullURL := openDotaBaseURL + path
	if enc := query.Encode(); enc != "" {
		fullURL += "?" + enc
	}

	// Enforce concurrency limit (max 3 simultaneous OpenDota requests)
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var lastErr error
	backoff := 800 * time.Millisecond
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "DotaScouter/1.0")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if attempt < 3 {
				select {
				case <-time.After(backoff):
					backoff *= 2
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, lastErr
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			lastErr = ErrRateLimited
			wait := backoff
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, perr := strconv.Atoi(ra); perr == nil && secs > 0 && secs <= 15 {
					wait = time.Duration(secs) * time.Second
				}
			}
			if attempt < 3 {
				select {
				case <-time.After(wait):
					backoff *= 2
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, ErrRateLimited
		}

		if resp.StatusCode >= 500 && attempt < 3 {
			lastErr = fmt.Errorf("OpenDota HTTP %d", resp.StatusCode)
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("OpenDota HTTP %d", resp.StatusCode)
		}
		if readErr != nil {
			return nil, readErr
		}
		return body, nil
	}

	return nil, lastErr
}
