package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"dotascouter/internal/api"
	"dotascouter/internal/config"
)

// verify_api runs a live OpenDota verification against a real public account (86745912)
// and a private/anonymous account (4294967295) during GitHub Actions CI.
func main() {
	cfg, err := config.NewManager()
	if err != nil {
		log.Fatalf("init config: %v", err)
	}

	scouter := api.NewScouter(cfg, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// 86745912 (Arteezy - public), 4294967295 (Anonymous/Private slot)
	ids := []int64{86745912, 4294967295}
	snap, err := scouter.ScoutAccountIDs(ctx, ids, "CI Live API Verification", true, nil)
	if err != nil {
		log.Fatalf("live API test failed: %v", err)
	}

	fmt.Println("=== LIVE OPENDOTA PUBLIC & PRIVATE API VERIFICATION ===")
	for _, p := range snap.Players {
		fmt.Printf("\nSlot #%d | AccountID=%d | Nickname=%s | Status=%s\n",
			p.Slot, p.AccountID, p.Nickname, p.ProfileStatus)
		if p.GamesCount > 0 {
			fmt.Printf("  Last %d: %d-%d (%.1f%%) | Avg KDA: %.1f\n",
				p.GamesCount, p.Wins, p.Losses, p.WinRate, p.AvgKDA)
			for i, h := range p.TopHeroes {
				fmt.Printf("  Top Hero %d: %s (%dG · %dW, %.1f%%) [%s]\n",
					i+1, h.HeroName, h.Games, h.Wins, h.WinRate, h.PortraitURL)
			}
		} else {
			fmt.Printf("  Fallback Detail: %s | MVP Status: %s\n", p.StatusDetail, p.MVPStatus)
		}
	}

	if len(snap.Players) != 2 {
		fmt.Fprintln(os.Stderr, "expected 2 player rows")
		os.Exit(1)
	}
	if snap.Players[0].Nickname == "" {
		fmt.Fprintln(os.Stderr, "public player returned empty nickname")
		os.Exit(1)
	}
	if snap.Players[1].ProfileStatus != "Private Profile" {
		fmt.Fprintf(os.Stderr, "expected Private Profile fallback, got %q\n", snap.Players[1].ProfileStatus)
		os.Exit(1)
	}
	fmt.Println("\nLive API verification PASSED.")
}
