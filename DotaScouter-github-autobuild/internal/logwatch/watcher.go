package logwatch

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	// Matches slot-indexed server_log.txt lobby entries like "0:[U:1:86745912]"
	slottedSteamID3Pattern = regexp.MustCompile(`\b([0-9]):\[U:1:(\d+)\]`)
	// Matches generic SteamID3 entries in console.log ("status" command) or server_log.txt
	steamID3Pattern = regexp.MustCompile(`\[U:1:(\d+)\]`)
)

// Watcher tails Dota 2's read-only console.log and server_log.txt every 500ms for live Picking Phase rosters.
type Watcher struct {
	mu            sync.RWMutex
	dotaRoot      string
	active        bool
	watchedPath   string
	offsets       map[string]int64
	lastSignature string
	onAccountIDs  func(ids []int64, source string)
}

// NewWatcher initializes a local Dota 2 log watcher.
func NewWatcher(dotaRoot string, onAccountIDs func([]int64, string)) *Watcher {
	return &Watcher{
		dotaRoot:     dotaRoot,
		offsets:      make(map[string]int64),
		onAccountIDs: onAccountIDs,
	}
}

// SetDotaRoot updates the Dota 2 folder path dynamically when the user changes Settings.
func (w *Watcher) SetDotaRoot(dotaRoot string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dotaRoot != dotaRoot {
		w.dotaRoot = dotaRoot
		w.offsets = make(map[string]int64)
	}
}

// Status reports whether a local Dota 2 log file is actively being watched.
func (w *Watcher) Status() (bool, string) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.active, w.watchedPath
}

// Run polls server_log.txt and console.log every 500ms for fast Picking Phase detection.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	// Prime offsets to end of existing files on startup so old sessions don't auto-trigger
	w.primeOffsets()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce()
		}
	}
}

// ScanLatestLobby reads the tail of server_log.txt and console.log on demand
// (used when GSI enters Hero Selection or when the user clicks "Scan Live Draft").
func (w *Watcher) ScanLatestLobby() ([]int64, string) {
	files := w.candidateFiles()
	for _, path := range files {
		tail := readTail(path, 64*1024)
		if tail == "" {
			continue
		}
		if ids := ExtractLatestLobbyIDs(tail); len(ids) >= 2 {
			return ids, "Live Draft (" + filepath.Base(path) + ")"
		}
	}
	return nil, ""
}

func (w *Watcher) candidateFiles() []string {
	w.mu.RLock()
	root := w.dotaRoot
	w.mu.RUnlock()
	if root == "" {
		return nil
	}
	return []string{
		filepath.Join(root, "game", "dota", "server_log.txt"),
		filepath.Join(root, "game", "dota", "console.log"),
	}
}

func (w *Watcher) primeOffsets() {
	files := w.candidateFiles()
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range files {
		if info, err := os.Stat(f); err == nil {
			w.offsets[f] = info.Size()
			w.active = true
			w.watchedPath = f
		}
	}
}

func (w *Watcher) pollOnce() {
	files := w.candidateFiles()
	if len(files) == 0 {
		w.mu.Lock()
		w.active = false
		w.watchedPath = ""
		w.mu.Unlock()
		return
	}

	foundAny := false
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		foundAny = true

		w.mu.Lock()
		w.active = true
		w.watchedPath = path
		prevOffset, known := w.offsets[path]
		if !known {
			w.offsets[path] = info.Size()
			w.mu.Unlock()
			continue
		}
		if info.Size() < prevOffset {
			prevOffset = 0
		}
		if info.Size() == prevOffset {
			w.mu.Unlock()
			continue
		}
		w.mu.Unlock()

		chunk, newOffset := readDelta(path, prevOffset)
		w.mu.Lock()
		w.offsets[path] = newOffset
		w.mu.Unlock()

		if chunk == "" {
			continue
		}

		ids := ExtractLatestLobbyIDs(chunk)
		if len(ids) >= 2 {
			sig := joinIDs(ids)
			w.mu.Lock()
			dup := sig == w.lastSignature
			if !dup {
				w.lastSignature = sig
			}
			w.mu.Unlock()

			if !dup && w.onAccountIDs != nil {
				go w.onAccountIDs(ids, "Live Draft ("+filepath.Base(path)+")")
			}
		}
	}

	if !foundAny {
		w.mu.Lock()
		w.active = false
		w.watchedPath = ""
		w.mu.Unlock()
	}
}

func readDelta(path string, offset int64) (string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return "", offset
	}
	defer f.Close()

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", offset
	}
	data, err := io.ReadAll(io.LimitReader(f, 512*1024))
	if err != nil {
		return "", offset
	}
	return string(data), offset + int64(len(data))
}

func readTail(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	start := info.Size() - maxBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return ""
	}
	return string(data)
}

// ExtractLatestLobbyIDs scans lines from newest to oldest to extract the latest
// Radiant (0-4) & Dire (5-9) lobby roster from server_log.txt or console.log.
func ExtractLatestLobbyIDs(text string) []int64 {
	lines := strings.Split(text, "\n")

	// Pass 1: Look from bottom up for a single server_log.txt lobby line with slotted entries (0:[U:1:...] .. 9:[U:1:...])
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		slotted := slottedSteamID3Pattern.FindAllStringSubmatch(line, -1)
		if len(slotted) >= 2 {
			slots := make([]int64, 10)
			count := 0
			for _, m := range slotted {
				idx, err1 := strconv.Atoi(m[1])
				id, err2 := strconv.ParseInt(m[2], 10, 64)
				if err1 == nil && err2 == nil && idx >= 0 && idx < 10 && id > 0 {
					slots[idx] = id
					count++
				}
			}
			if count >= 2 {
				// Return compact slice if fewer than 10, or full 10-slot Radiant(0-4)/Dire(5-9) array
				if count == 10 {
					return slots
				}
				compact := make([]int64, 0, count)
				for _, id := range slots {
					if id > 0 {
						compact = append(compact, id)
					}
				}
				return compact
			}
		}
	}

	// Pass 2: Look from bottom up across recent lines for a console.log "status" block of [U:1:XXXXXXXX]
	seen := make(map[int64]bool)
	rev := make([]int64, 0, 10)
	for i := len(lines) - 1; i >= 0 && len(rev) < 10; i-- {
		matches := steamID3Pattern.FindAllStringSubmatch(lines[i], -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			id, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil || id <= 0 || seen[id] {
				continue
			}
			seen[id] = true
			rev = append(rev, id)
			if len(rev) == 10 {
				break
			}
		}
	}
	// Reverse back into chronological (Radiant -> Dire) order
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}
