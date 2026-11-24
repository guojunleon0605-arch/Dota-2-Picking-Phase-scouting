package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const stratzGraphQLEndpoint = "https://api.stratz.com/graphql"

// StratzClient queries STRATZ's public GraphQL API with the mandatory User-Agent: STRATZ_API header.
type StratzClient struct {
	httpClient *http.Client
	sem        chan struct{}
}

// NewStratzClient initializes a concurrency-bounded STRATZ GraphQL client.
func NewStratzClient() *StratzClient {
	return &StratzClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		sem:        make(chan struct{}, 3),
	}
}

// StratzMVPResult holds MVP count and parsed match coverage for a player's last 20 ranked matches.
type StratzMVPResult struct {
	MVPCount    int
	ParsedCount int
	TotalCount  int
	Status      string // "OK", "Private Profile", "API 429 Rate Limited", "Token Invalid", etc.
}

type graphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

// TestToken validates a user-supplied STRATZ Bearer token against https://api.stratz.com/graphql.
func (c *StratzClient) TestToken(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("STRATZ token is empty")
	}

	reqBody := graphQLRequest{
		Query: `query TestToken { __typename }`,
	}
	status, raw, err := c.postGraphQL(ctx, token, reqBody)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", fmt.Errorf("STRATZ rejected token (HTTP %d)", status)
	}
	if status == http.StatusTooManyRequests {
		return "", ErrRateLimited
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("STRATZ HTTP %d", status)
	}

	var parsed struct {
		Data   map[string]interface{} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("invalid STRATZ response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		return "", fmt.Errorf("STRATZ GraphQL error: %s", parsed.Errors[0].Message)
	}
	return "STRATZ API token is valid and active.", nil
}

// GetPlayerMVPs fetches a player's last 20 ranked matches (lobbyTypeIds: [7]) from STRATZ
// to compute MVP awards and parsed match coverage.
func (c *StratzClient) GetPlayerMVPs(ctx context.Context, token string, accountID int64) StratzMVPResult {
	token = strings.TrimSpace(token)
	if token == "" {
		return StratzMVPResult{Status: "Add Token"}
	}

	query := `
	query PlayerMVPs($steamAccountId: Long!) {
		player(steamAccountId: $steamAccountId) {
			steamAccount {
				isAnonymous
			}
			matches(request: { lobbyTypeIds: [7], take: 20 }) {
				id
				parsedDateTime
				players(steamAccountId: $steamAccountId) {
					steamAccountId
					award
				}
			}
		}
	}`

	reqBody := graphQLRequest{
		Query: query,
		Variables: map[string]interface{}{
			"steamAccountId": accountID,
		},
	}

	status, raw, err := c.postGraphQL(ctx, token, reqBody)
	if err != nil {
		return StratzMVPResult{Status: "N/A"}
	}
	if status == http.StatusTooManyRequests {
		return StratzMVPResult{Status: "API 429 Rate Limited"}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return StratzMVPResult{Status: "Invalid Token"}
	}
	if status != http.StatusOK {
		return StratzMVPResult{Status: fmt.Sprintf("HTTP %d", status)}
	}

	var resp struct {
		Data struct {
			Player *struct {
				SteamAccount *struct {
					IsAnonymous bool `json:"isAnonymous"`
				} `json:"steamAccount"`
				Matches []struct {
					ID             int64  `json:"id"`
					ParsedDateTime *int64 `json:"parsedDateTime"`
					Players        []struct {
						SteamAccountID int64  `json:"steamAccountId"`
						Award          string `json:"award"`
					} `json:"players"`
				} `json:"matches"`
			} `json:"player"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Errors) > 0 || resp.Data.Player == nil {
		return StratzMVPResult{Status: "N/A"}
	}

	if resp.Data.Player.SteamAccount != nil && resp.Data.Player.SteamAccount.IsAnonymous && len(resp.Data.Player.Matches) == 0 {
		return StratzMVPResult{Status: "Private Profile"}
	}

	total := len(resp.Data.Player.Matches)
	if total == 0 {
		return StratzMVPResult{Status: "N/A"}
	}

	parsedCount := 0
	mvpCount := 0
	for _, m := range resp.Data.Player.Matches {
		if m.ParsedDateTime != nil && *m.ParsedDateTime > 0 {
			parsedCount++
		}
		for _, p := range m.Players {
			if p.SteamAccountID == accountID {
				award := strings.ToUpper(strings.TrimSpace(p.Award))
				if award == "MVP" || award == "VSMVP" {
					mvpCount++
				}
			}
		}
	}

	return StratzMVPResult{
		MVPCount:    mvpCount,
		ParsedCount: parsedCount,
		TotalCount:  total,
		Status:      "OK",
	}
}

// GetLatestMatchID returns the most recent match ID for a player via STRATZ as a fallback.
func (c *StratzClient) GetLatestMatchID(ctx context.Context, token string, accountID int64) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, fmt.Errorf("no STRATZ token")
	}

	reqBody := graphQLRequest{
		Query: `
		query LatestMatch($steamAccountId: Long!) {
			player(steamAccountId: $steamAccountId) {
				matches(request: { take: 1 }) {
					id
				}
			}
		}`,
		Variables: map[string]interface{}{"steamAccountId": accountID},
	}

	status, raw, err := c.postGraphQL(ctx, token, reqBody)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("STRATZ HTTP %d", status)
	}

	var resp struct {
		Data struct {
			Player *struct {
				Matches []struct {
					ID int64 `json:"id"`
				} `json:"matches"`
			} `json:"player"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Data.Player == nil || len(resp.Data.Player.Matches) == 0 {
		return 0, fmt.Errorf("no STRATZ matches found for %d", accountID)
	}
	return resp.Data.Player.Matches[0].ID, nil
}

func (c *StratzClient) postGraphQL(ctx context.Context, token string, payload graphQLRequest) (int, []byte, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stratzGraphQLEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, err
	}
	// Mandatory STRATZ/Cloudflare headers
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "STRATZ_API")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	return resp.StatusCode, body, err
}
