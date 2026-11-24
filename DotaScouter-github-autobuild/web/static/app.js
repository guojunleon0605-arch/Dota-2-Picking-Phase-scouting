(() => {
  const state = {
    settings: {},
    status: {},
    currentSnapshot: null,
    historySnapshot: null,
    viewMode: "current", // "current" | "snapshot"
    showID: true,
    queryTab: "match"
  };

  const $ = (id) => document.getElementById(id);

  function showToast(msg) {
    const el = $("toast");
    el.textContent = msg;
    el.classList.remove("hidden");
    clearTimeout(showToast._t);
    showToast._t = setTimeout(() => el.classList.add("hidden"), 3200);
  }

  function escapeHTML(str) {
    return String(str ?? "")
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  async function apiFetch(url, opts = {}) {
    const res = await fetch(url, {
      headers: { "Content-Type": "application/json" },
      ...opts
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data.error || `HTTP ${res.status}`);
    }
    return data;
  }

  function switchTab(tab) {
    document.querySelectorAll(".nav-item").forEach((b) => {
      b.classList.toggle("active", b.dataset.tab === tab);
    });
    document.querySelectorAll(".tab-panel").forEach((p) => {
      p.classList.toggle("active", p.id === `tab-${tab}`);
    });
    if (tab === "history") {
      loadHistoryList();
    }
  }

  function updateStatusUI(st) {
    if (!st) return;
    state.status = st;

    const gsiText = st.gsi_connected
      ? `Dota 2 Connected${st.gsi_game_state ? " · " + st.gsi_game_state.replace("DOTA_GAMERULES_STATE_", "") : ""}`
      : st.gsi_listening
        ? `GSI: Listening on :${st.gsi_port || 44444}`
        : "GSI: Offline";

    $("top-gsi-pill").textContent = gsiText;
    $("top-gsi-pill").className = `pill ${st.gsi_connected ? "pill-green" : "pill-purple"}`;
    $("top-pick-pill").classList.toggle("hidden", !st.picking_phase);

    $("sidebar-gsi-text").textContent = gsiText;
    $("sidebar-gsi-dot").className = `dot ${st.gsi_connected ? "dot-ok" : st.gsi_listening ? "dot-idle" : "dot-err"}`;

    const counter = `${st.players_loaded || 0}/${st.players_total || 0} players loaded`;
    $("top-load-pill").textContent = counter;
    $("bot-counter").textContent = counter;

    const diag = st.diagnostics || "Ready";
    $("top-diag-text").textContent = diag;
    $("bot-diag").textContent = diag;
    $("bot-gsi").textContent = gsiText;
    $("bot-log").textContent = st.log_watching
      ? `Log Watcher: Active (500ms)`
      : "Log Watcher: Waiting for Dota 2 logs";

    const busy = !!st.is_loading;
    $("btn-sync-latest").disabled = busy;
    $("btn-refresh").disabled = busy;
    $("btn-scan-lobby").disabled = busy;
  }

  function activeSnapshot() {
    return state.viewMode === "snapshot" ? state.historySnapshot : state.currentSnapshot;
  }

  function renderMatchTable() {
    const snap = activeSnapshot();
    $("view-current").classList.toggle("active", state.viewMode === "current");
    $("view-snapshot").classList.toggle("active", state.viewMode === "snapshot");

    if (!snap || !Array.isArray(snap.players) || snap.players.length === 0) {
      $("match-empty").classList.remove("hidden");
      $("tables-wrap").classList.add("hidden");
      $("top-match-pill").classList.add("hidden");
      return;
    }

    $("match-empty").classList.add("hidden");
    $("tables-wrap").classList.remove("hidden");
    $("tables-wrap").classList.toggle("hide-id", !state.showID);

    const matchLabel = snap.match_id > 0
      ? `Match #${snap.match_id} (${snap.source})`
      : `${snap.source} (${snap.players.length} players)`;
    $("top-match-pill").textContent = matchLabel;
    $("top-match-pill").classList.remove("hidden");

    const radiant = snap.players.filter((p) => p.team === "Radiant");
    const dire = snap.players.filter((p) => p.team === "Dire");

    $("tbody-radiant").innerHTML = radiant.map(renderPlayerRow).join("");
    $("tbody-dire").innerHTML = dire.map(renderPlayerRow).join("");

    $("radiant-summary").textContent = teamSummaryLine(radiant);
    $("dire-summary").textContent = teamSummaryLine(dire);

    document.querySelectorAll(".ext-link").forEach((el) => {
      el.addEventListener("click", (e) => {
        e.preventDefault();
        const url = el.dataset.url;
        if (url) {
          apiFetch("/api/open-url", { method: "POST", body: JSON.stringify({ url }) }).catch(() => {
            window.open(url, "_blank", "noopener");
          });
        }
      });
    });
  }

  function teamSummaryLine(players) {
    const valid = players.filter((p) => p.games_count > 0);
    if (valid.length === 0) return "";
    const avgWR = valid.reduce((a, b) => a + b.win_rate, 0) / valid.length;
    const avgKDA = valid.reduce((a, b) => a + b.avg_kda, 0) / valid.length;
    return `Avg WR ${avgWR.toFixed(1)}% · Avg KDA ${avgKDA.toFixed(1)}`;
  }

  function renderPlayerRow(p) {
    const avatar = p.avatar_url
      ? `<img class="avatar" src="${escapeHTML(p.avatar_url)}" alt="" loading="lazy" />`
      : `<div class="avatar"></div>`;

    const rankBadge = p.rank_label ? `<span class="rank-tag">${escapeHTML(p.rank_label)}</span>` : "";
    const idTag = p.account_id > 0 ? `<span class="id-tag">ID: ${p.account_id}</span>` : `<span class="id-tag">Anonymous</span>`;

    const tags = [];
    if (p.encounter && p.encounter.summary_tag) {
      const heroHint = p.encounter.recent_heroes?.length ? ` (${p.encounter.recent_heroes.join(", ")})` : "";
      tags.push(`<span class="tag tag-encounter">${escapeHTML(p.encounter.summary_tag + heroHint)}</span>`);
    }
    if (p.steam) {
      if (p.steam.created_year > 0) {
        tags.push(`<span class="tag tag-steam">Steam ${p.steam.created_year}</span>`);
      }
      if (p.steam.dota_hours > 0) {
        tags.push(`<span class="tag tag-steam">${p.steam.dota_hours.toFixed(0)}h Dota</span>`);
      }
    }

    const links = p.account_id > 0 && p.links ? `
      <div class="ext-links">
        <a class="ext-link" href="#" data-url="${escapeHTML(p.links.opendota)}">OpenDota</a>
        <a class="ext-link" href="#" data-url="${escapeHTML(p.links.stratz)}">STRATZ</a>
        <a class="ext-link" href="#" data-url="${escapeHTML(p.links.dotabuff)}">Dotabuff</a>
        <a class="ext-link" href="#" data-url="${escapeHTML(p.links.steam)}">Steam</a>
      </div>` : "";

    let last20HTML = "";
    if (p.games_count > 0) {
      const wrCls = p.win_rate >= 50 ? "wr-green" : "wr-coral";
      const archBadge = p.profile_status === "Archived"
        ? `<div><span class="badge-amber">Archived${p.archived_date ? " (Last public: " + escapeHTML(p.archived_date) + ")" : ""}</span></div>`
        : "";
      last20HTML = `
        <div class="wl-main">${p.wins}-${p.losses} <span class="${wrCls}">(${p.win_rate.toFixed(0)}%)</span></div>
        ${archBadge}
      `;
    } else {
      last20HTML = `<span class="badge-private">${escapeHTML(p.profile_status || "Private Profile")}</span>`;
    }

    const kdaHTML = p.games_count > 0 ? p.avg_kda.toFixed(1) : `<span class="mvp-muted">—</span>`;

    let heroesHTML = `<span class="mvp-muted">${escapeHTML(p.status_detail || "No public hero data")}</span>`;
    if (Array.isArray(p.top_heroes) && p.top_heroes.length > 0) {
      heroesHTML = `<div class="heroes-row">` + p.top_heroes.map((h) => {
        const wrCls = h.win_rate >= 50 ? "wr-green" : "wr-coral";
        const img = h.portrait_url
          ? `<img class="hero-img" src="${escapeHTML(h.portrait_url)}" alt="${escapeHTML(h.hero_name)}" title="${escapeHTML(h.hero_name)}" loading="lazy" />`
          : "";
        return `
          <div class="hero-chip" title="${escapeHTML(h.hero_name)}">
            ${img}
            <div class="hero-stats">
              <span class="hero-gw">${h.games}G · ${h.wins}W</span>
              <span class="hero-wr ${wrCls}">${h.win_rate.toFixed(1)}%</span>
            </div>
          </div>`;
      }).join("") + `</div>`;
    }

    let mvpHTML = `<span class="mvp-muted">${escapeHTML(p.mvp_status || "Add Token")}</span>`;
    if (p.mvp_status === "OK") {
      mvpHTML = `
        <div class="mvp-num">${p.mvp_count}</div>
        <div class="mvp-sub">${p.mvp_parsed}/${p.mvp_total} parsed</div>
      `;
    }

    return `
      <tr>
        <td class="col-slot">${p.slot}</td>
        <td class="col-player">
          <div class="player-cell">
            ${avatar}
            <div class="player-info">
              <div class="player-name-row">
                <span class="player-name">${escapeHTML(p.nickname)}</span>
                ${rankBadge}
                ${idTag}
              </div>
              ${tags.length ? `<div class="meta-tags">${tags.join("")}</div>` : ""}
              ${links}
            </div>
          </div>
        </td>
        <td class="col-last20">${last20HTML}</td>
        <td class="col-kda">${kdaHTML}</td>
        <td class="col-heroes">${heroesHTML}</td>
        <td class="col-mvp">${mvpHTML}</td>
      </tr>
    `;
  }

  async function loadHistoryList() {
    const container = $("history-list");
    container.innerHTML = `<div class="muted">Loading saved snapshots...</div>`;
    try {
      const items = await apiFetch("/api/history");
      if (!items.length) {
        container.innerHTML = `<div class="muted">No saved match snapshots yet. Scout a match to build your local encounter history.</div>`;
        return;
      }
      container.innerHTML = items.map((item) => {
        const dt = item.created_at ? new Date(item.created_at).toLocaleString() : "";
        const title = item.match_id > 0 ? `Match #${item.match_id}` : `Live Draft / Player Query`;
        return `
          <div class="history-item" data-id="${item.id}">
            <div>
              <strong>${escapeHTML(title)}</strong>
              <span class="pill pill-purple" style="margin-left:8px">${escapeHTML(item.source)}</span>
              <div class="muted" style="margin-top:3px">${item.players_count} players · ${escapeHTML(dt)}</div>
            </div>
            <button class="btn btn-secondary btn-sm">Load Snapshot</button>
          </div>
        `;
      }).join("");

      container.querySelectorAll(".history-item").forEach((el) => {
        el.addEventListener("click", async () => {
          const id = el.dataset.id;
          try {
            const snap = await apiFetch(`/api/history/${id}`);
            state.historySnapshot = snap;
            state.viewMode = "snapshot";
            switchTab("match");
            renderMatchTable();
            showToast(`Loaded snapshot #${id}`);
          } catch (err) {
            showToast(`Failed to load snapshot: ${err.message}`);
          }
        });
      });
    } catch (err) {
      container.innerHTML = `<div class="feedback-box feedback-err">${escapeHTML(err.message)}</div>`;
    }
  }

  function populateSettingsForm(s, detectedPath) {
    state.settings = s || {};
    state.showID = s.show_id_column !== false;
    $("set-friend-id").value = s.my_friend_id || "";
    $("set-dota-path").value = s.dota2_path || "";
    $("set-stratz-token").value = s.stratz_token || "";
    $("set-opendota-key").value = s.opendota_api_key || "";
    $("set-steam-key").value = s.steam_api_key || "";
    $("toggle-autosync").checked = !!s.auto_sync_enabled;
    $("detected-path-hint").textContent = detectedPath
      ? `Auto-detected: ${detectedPath}`
      : "Auto-detected: Dota 2 folder not found automatically (enter path manually)";
  }

  function buildCopySummary() {
    const snap = activeSnapshot();
    if (!snap || !snap.players?.length) return "";
    const lines = [];
    lines.push(snap.match_id > 0 ? `DotaScouter — Match #${snap.match_id}` : `DotaScouter — ${snap.source}`);
    ["Radiant", "Dire"].forEach((team) => {
      const list = snap.players.filter((p) => p.team === team);
      if (!list.length) return;
      lines.push(`\n[${team.toUpperCase()}]`);
      list.forEach((p) => {
        const stat = p.games_count > 0
          ? `${p.wins}-${p.losses} (${p.win_rate.toFixed(0)}%) | KDA ${p.avg_kda.toFixed(1)}`
          : p.profile_status;
        const heroes = (p.top_heroes || []).map((h) => `${h.hero_name} ${h.games}G/${h.wins}W`).join(", ");
        const mvp = p.mvp_status === "OK" ? ` | MVP ${p.mvp_count} (${p.mvp_parsed}/${p.mvp_total})` : "";
        lines.push(`#${p.slot} ${p.nickname}: ${stat}${heroes ? " | Top: " + heroes : ""}${mvp}`);
      });
    });
    return lines.join("\n");
  }

  async function runScanLobby() {
    try {
      const snap = await apiFetch("/api/scout/scan-lobby", { method: "POST" });
      state.currentSnapshot = snap;
      state.viewMode = "current";
      switchTab("match");
      renderMatchTable();
      showToast(`Loaded ${snap.players.length} live draft players!`);
    } catch (err) {
      showToast(err.message);
    }
  }

  document.querySelectorAll(".nav-item").forEach((btn) => {
    btn.addEventListener("click", () => switchTab(btn.dataset.tab));
  });

  $("view-current").addEventListener("click", () => {
    state.viewMode = "current";
    renderMatchTable();
  });

  $("view-snapshot").addEventListener("click", () => {
    if (!state.historySnapshot) {
      switchTab("history");
      showToast("Pick a saved match from History Snapshots.");
      return;
    }
    state.viewMode = "snapshot";
    renderMatchTable();
  });

  $("btn-toggle-id").addEventListener("click", () => {
    state.showID = !state.showID;
    $("tables-wrap").classList.toggle("hide-id", !state.showID);
  });

  $("btn-copy-summary").addEventListener("click", async () => {
    const text = buildCopySummary();
    if (!text) {
      showToast("No match data loaded to copy.");
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      showToast("Team scouting summary copied to clipboard.");
    } catch {
      showToast("Clipboard write failed.");
    }
  });

  $("btn-scan-lobby").addEventListener("click", runScanLobby);
  $("empty-btn-scan").addEventListener("click", runScanLobby);

  $("btn-sync-latest").addEventListener("click", async () => {
    try {
      const snap = await apiFetch("/api/scout/sync-latest", { method: "POST" });
      state.currentSnapshot = snap;
      state.viewMode = "current";
      renderMatchTable();
      showToast(`Synced Match #${snap.match_id}`);
    } catch (err) {
      showToast(err.message);
    }
  });

  $("btn-refresh").addEventListener("click", async () => {
    try {
      const snap = await apiFetch("/api/scout/refresh", { method: "POST" });
      state.currentSnapshot = snap;
      state.viewMode = "current";
      renderMatchTable();
      showToast("Refreshed live data from APIs.");
    } catch (err) {
      showToast(err.message);
    }
  });

  $("toggle-autosync").addEventListener("change", async (e) => {
    const next = { ...state.settings, auto_sync_enabled: e.target.checked };
    try {
      const res = await apiFetch("/api/settings", { method: "POST", body: JSON.stringify(next) });
      state.settings = res.settings;
      showToast(e.target.checked ? "30s Auto-Sync enabled." : "30s Auto-Sync paused.");
    } catch (err) {
      e.target.checked = !e.target.checked;
      showToast(err.message);
    }
  });

  // Query Modal
  function openModal() {
    $("query-error").classList.add("hidden");
    $("query-modal").classList.remove("hidden");
  }
  function closeModal() {
    $("query-modal").classList.add("hidden");
  }
  $("btn-open-query").addEventListener("click", openModal);
  $("empty-btn-query").addEventListener("click", openModal);
  $("empty-btn-settings").addEventListener("click", () => switchTab("settings"));
  $("btn-close-modal").addEventListener("click", closeModal);
  $("btn-cancel-modal").addEventListener("click", closeModal);

  $("qtab-match").addEventListener("click", () => {
    state.queryTab = "match";
    $("qtab-match").classList.add("active");
    $("qtab-ids").classList.remove("active");
    $("qpanel-match").classList.remove("hidden");
    $("qpanel-ids").classList.add("hidden");
  });

  $("qtab-ids").addEventListener("click", () => {
    state.queryTab = "ids";
    $("qtab-ids").classList.add("active");
    $("qtab-match").classList.remove("active");
    $("qpanel-ids").classList.remove("hidden");
    $("qpanel-match").classList.add("hidden");
  });

  $("btn-run-query").addEventListener("click", async () => {
    const errBox = $("query-error");
    errBox.classList.add("hidden");
    $("btn-run-query").disabled = true;
    try {
      let snap;
      if (state.queryTab === "match") {
        snap = await apiFetch("/api/scout/match", {
          method: "POST",
          body: JSON.stringify({ match_id: $("input-match-id").value.trim() })
        });
      } else {
        snap = await apiFetch("/api/scout/players", {
          method: "POST",
          body: JSON.stringify({ raw_ids: $("input-player-ids").value.trim() })
        });
      }
      state.currentSnapshot = snap;
      state.viewMode = "current";
      closeModal();
      switchTab("match");
      renderMatchTable();
      showToast("Live scouting complete.");
    } catch (err) {
      errBox.textContent = err.message;
      errBox.classList.remove("hidden");
    } finally {
      $("btn-run-query").disabled = false;
    }
  });

  // Settings form
  $("settings-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const fb = $("settings-save-feedback");
    fb.classList.add("hidden");
    const payload = {
      ...state.settings,
      my_friend_id: $("set-friend-id").value.trim(),
      dota2_path: $("set-dota-path").value.trim(),
      stratz_token: $("set-stratz-token").value.trim(),
      opendota_api_key: $("set-opendota-key").value.trim(),
      steam_api_key: $("set-steam-key").value.trim(),
      auto_sync_enabled: $("toggle-autosync").checked,
      show_id_column: state.showID
    };
    try {
      const res = await apiFetch("/api/settings", { method: "POST", body: JSON.stringify(payload) });
      state.settings = res.settings;
      fb.className = "feedback-box";
      fb.textContent = "Settings saved.";
      fb.classList.remove("hidden");
      showToast("Settings saved.");
    } catch (err) {
      fb.className = "feedback-box feedback-err";
      fb.textContent = err.message;
      fb.classList.remove("hidden");
    }
  });

  $("btn-test-stratz").addEventListener("click", async () => {
    const fb = $("stratz-test-feedback");
    fb.className = "feedback-box";
    fb.textContent = "Testing STRATZ GraphQL token...";
    fb.classList.remove("hidden");
    try {
      const res = await apiFetch("/api/settings/test-stratz", {
        method: "POST",
        body: JSON.stringify({ token: $("set-stratz-token").value.trim() })
      });
      fb.className = res.ok ? "feedback-box" : "feedback-box feedback-err";
      fb.textContent = res.message;
    } catch (err) {
      fb.className = "feedback-box feedback-err";
      fb.textContent = err.message;
    }
  });

  $("btn-install-gsi").addEventListener("click", async () => {
    const fb = $("gsi-install-feedback");
    fb.classList.add("hidden");
    try {
      const res = await apiFetch("/api/settings/install-gsi", {
        method: "POST",
        body: JSON.stringify({ dota2_path: $("set-dota-path").value.trim() })
      });
      fb.className = "feedback-box";
      fb.textContent = `Installed: ${res.path} — ${res.reminder}`;
      fb.classList.remove("hidden");
    } catch (err) {
      fb.className = "feedback-box feedback-err";
      fb.textContent = err.message;
      fb.classList.remove("hidden");
    }
  });

  $("btn-clear-cache").addEventListener("click", async () => {
    try {
      const res = await apiFetch("/api/settings/clear-cache", { method: "POST" });
      showToast(res.message || "Cache cleared.");
    } catch (err) {
      showToast(err.message);
    }
  });

  $("btn-reload-history").addEventListener("click", loadHistoryList);

  $("btn-quit").addEventListener("click", () => {
    fetch("/api/shutdown", { method: "POST" }).finally(() => window.close());
  });

  // Live SSE updates from Go backend
  function connectSSE() {
    const es = new EventSource("/api/events");
    es.onmessage = (e) => {
      try {
        const msg = JSON.parse(e.data);
        if (msg.type === "status") {
          updateStatusUI(msg.data);
        } else if (msg.type === "snapshot") {
          state.currentSnapshot = msg.data;
          if (state.viewMode === "current") {
            renderMatchTable();
          }
        }
      } catch {
        // ignore malformed SSE chunk
      }
    };
    es.onerror = () => {
      es.close();
      setTimeout(connectSSE, 3000);
    };
  }

  async function init() {
    try {
      const data = await apiFetch("/api/state");
      populateSettingsForm(data.settings, data.detected_dota_path);
      updateStatusUI(data.status);
      if (data.snapshot) {
        state.currentSnapshot = data.snapshot;
      }
      renderMatchTable();
    } catch (err) {
      showToast(`Startup error: ${err.message}`);
    }
    connectSSE();
  }

  init();
})();
