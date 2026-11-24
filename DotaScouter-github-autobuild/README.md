# DotaScouter — Auto-Build Windows Package (Live Picking Phase Edition)

Standalone, 100% VAC-safe Dota 2 match scouter built in pure Go (`//go:embed` frontend + pure-Go SQLite via `modernc.org/sqlite`).

## Option A: Build `DotaScouter.exe` in the Cloud via GitHub Actions (Zero Local Setup)

1. Create a new repository on [GitHub](https://github.com/new) (public or private).
2. Unzip `DotaScouter-github-autobuild.zip` and upload all files (including the `.github` folder) to the repository's `main` branch.
3. Open the **Actions** tab → click **Build & Verify DotaScouter.exe (Windows PE32+ GUI)**.
   - The workflow automatically runs the live OpenDota public & private API verification (`cmd/verify_api`), compiles the Windows GUI binary (`GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-H windowsgui -s -w"`), verifies `PE32+ executable (GUI)` + file size (`> 5 MB`) + SHA-256 checksum, and attaches **`DotaScouter-Windows-x64`** under **Artifacts**.
4. Click **DotaScouter-Windows-x64** to download `DotaScouter.exe` and `DotaScouter.exe.sha256` directly.

*(Tip: Pushing a tag like `v1.0.0` also creates a permanent GitHub Release with `DotaScouter.exe` attached.)*

---

## Live Dota 2 Picking Phase Setup (VAC-Safe)

OpenDota only indexes matches *after* they end, so during the **Hero Selection / Picking Phase**, DotaScouter detects all 10 players live using read-only Game State Integration (GSI) + local Dota 2 logs:

1. Open `DotaScouter.exe` → **Settings & Connection** → click **1-Click Install GSI Config**.
2. In Steam, right-click **Dota 2 → Properties → General → Launch Options** and add:
   ```text
   -gamestateintegration -condebug
   ```
3. Restart Dota 2. When a match is accepted and enters **Hero Selection**, DotaScouter lights up **LIVE PICKING PHASE**, scans `server_log.txt` / `console.log` every 500ms for all 10 Radiant/Dire SteamID3s (`0:[U:1:...] .. 9:[U:1:...]`), and loads every player's Last 20 Win Rate, KDA, Top 3 Heroes, Local Encounter Memory, and STRATZ MVPs automatically.
4. *(Optional backup during draft)* If `server_log.txt` hasn't flushed yet, press your console key in Dota 2, type `status`, and DotaScouter will pick up all 10 players instantly (or click **Scan Live Draft**).
