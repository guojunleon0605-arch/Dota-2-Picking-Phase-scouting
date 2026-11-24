package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SteamClient queries the optional public Steam Web API for account age & Dota 2 playtime.
type SteamClient struct {
	httpClient *http.Client
}

// NewSteamClient creates a Steam Web API client.
func NewSteamClient() *SteamClient {
	return &SteamClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// SteamSummaryData holds persona, avatar, and account creation year from GetPlayerSummaries.
type SteamSummaryData struct {
	PersonaName string
	AvatarFull  string
	ProfileURL  string
	CreatedYear int
}

// GetPlayerSummariesBatch queries ISteamUser/GetPlayerSummaries/v0002 for up to 100 SteamID64s in a single call.
func (c *SteamClient) GetPlayerSummariesBatch(ctx context.Context, apiKey string, steamIDs []int64) map[int64]SteamSummaryData {
	out := make(map[int64]SteamSummaryData)
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || len(steamIDs) == 0 {
		return out
	}

	parts := make([]string, 0, len(steamIDs))
	for _, id := range steamIDs {
		if id > 0 {
			parts = append(parts, strconv.FormatInt(id, 10))
		}
	}
	if len(parts) == 0 {
		return out
	}

	u := fmt.Sprintf(
		"https://api.steampowered.com/ISteamUser/GetPlayerSummaries/v0002/?key=%s&steamids=%s",
		url.QueryEscape(apiKey),
		url.QueryEscape(strings.Join(parts, ",")),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return out
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return out
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return out
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return out
	}

	var parsed struct {
		Response struct {
			Players []struct {
				SteamID     string `json:"steamid"`
				PersonaName string `json:"personaname"`
				AvatarFull  string `json:"avatarfull"`
				ProfileURL  string `json:"profileurl"`
				TimeCreated int64  `json:"timecreated"`
			} `json:"players"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return out
	}

	for _, p := range parsed.Response.Players {
		id64, err := strconv.ParseInt(p.SteamID, 10, 64)
		if err != nil {
			continue
		}
		yr := 0
		if p.TimeCreated > 0 {
			yr = time.Unix(p.TimeCreated, 0).UTC().Year()
		}
		out[id64] = SteamSummaryData{
			PersonaName: p.PersonaName,
			AvatarFull:  p.AvatarFull,
			ProfileURL:  p.ProfileURL,
			CreatedYear: yr,
		}
	}
	return out
}

// GetDota2Hours queries IPlayerService/GetOwnedGames/v0001 filtered to AppID 570 (Dota 2).
func (c *SteamClient) GetDota2Hours(ctx context.Context, apiKey string, steamID64 int64) float64 {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || steamID64 <= 0 {
		return 0
	}

	u := fmt.Sprintf(
		"https://api.steampowered.com/IPlayerService/GetOwnedGames/v0001/?key=%s&steamid=%d&include_played_free_games=1&appids_filter[0]=570&format=json",
		url.QueryEscape(apiKey),
		steamID64,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return 0
	}

	var parsed struct {
		Response struct {
			Games []struct {
				AppID           int `json:"appid"`
				PlaytimeForever int `json:"playtime_forever"`
			} `json:"games"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0
	}
	for _, g := range parsed.Response.Games {
		if g.AppID == 570 && g.PlaytimeForever > 0 {
			return float64(g.PlaytimeForever) / 60.0
		}
	}
	return 0
}
