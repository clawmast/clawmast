// ClawMast walking-skeleton dashboard client.
//
// No framework, no build step — deliberately. The page has one status
// card and one "check for updates" button, so the JS only needs to
// fetch three endpoints and render five fields.
//
// Later iterations replace this file with a React + Vite bundle; the
// embed FS shape stays the same (architecture/refactor.md §9).

(() => {
  const $ = (id) => document.getElementById(id);

  const versionEl = $("version");
  const buildEl = $("build");
  const uptimeEl = $("uptime");
  const fullEl = $("footer-full");
  const statusBadge = $("status-badge");
  const statusText = $("status-text");
  const checkBtn = $("check-updates");
  const updateResult = $("update-result");
  const updateResultText = $("update-result-text");
  const updateNote = $("update-note");
  const historyCard = $("history-card");
  const historyList = $("history-list");
  const historyEmpty = $("history-empty");
  const historyHint = $("history-hint");
  const historyRefreshBtn = $("history-refresh");
  const blacklistCard = $("blacklist-card");
  const blacklistList = $("blacklist-list");
  const blacklistEmpty = $("blacklist-empty");
  const blacklistResult = $("blacklist-result");
  const markBadBtn = $("mark-bad");

  let currentVersion = null;

  const rtf = new Intl.RelativeTimeFormat("zh-Hans", { numeric: "auto" });
  const tfmt = new Intl.DateTimeFormat("zh-Hans", {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
  });

  async function jfetch(url, init) {
    const res = await fetch(url, init);
    if (!res.ok) {
      throw new Error(`${res.status} ${res.statusText}`);
    }
    return res.json();
  }

  function fmtUptime(ms) {
    if (!Number.isFinite(ms) || ms < 0) return "—";
    const s = Math.floor(ms / 1000);
    if (s < 60) return `${s} 秒`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m} 分 ${s % 60} 秒`;
    const h = Math.floor(m / 60);
    return `${h} 时 ${m % 60} 分`;
  }

  function setStatus(kind, text) {
    statusBadge.classList.remove("badge-unknown", "badge-healthy", "badge-error");
    statusBadge.classList.add(`badge-${kind}`);
    statusText.textContent = text;
  }

  async function loadVersion() {
    try {
      const v = await jfetch("/api/version");
      // Prefer the supervisor-assigned label (matches what the
      // installer and blacklist use); fall back to the ldflags
      // version in standalone mode.
      currentVersion = v.label || v.version || "";
      versionEl.textContent = currentVersion || "dev";
      buildEl.textContent = `${v.commit || "none"} · ${v.build_time || "unknown"}`;
      fullEl.textContent = v.full || "";
    } catch (err) {
      versionEl.textContent = "加载失败";
      buildEl.textContent = String(err.message || err);
    }
  }

  async function poll() {
    try {
      const h = await jfetch("/api/health");
      setStatus(h.ok ? "healthy" : "error", h.ok ? "运行中" : "异常");
      uptimeEl.textContent = fmtUptime(h.uptime_ms);
    } catch (err) {
      setStatus("error", "无法连接");
      uptimeEl.textContent = "—";
    }
  }

  async function checkUpdates() {
    checkBtn.dataset.loading = "1";
    checkBtn.disabled = true;
    try {
      const u = await jfetch("/api/updates/check", { method: "POST" });
      updateResult.hidden = false;
      const label = u.update_available
        ? `有新版本:${u.latest}(当前 ${u.current})`
        : `已是最新:${u.current}`;
      updateResultText.textContent = label;
      if (u.note) updateNote.textContent = u.note;
    } catch (err) {
      updateResult.hidden = false;
      updateResultText.textContent = `检查失败:${err.message || err}`;
    } finally {
      checkBtn.dataset.loading = "0";
      checkBtn.disabled = false;
    }
  }

  // --- history timeline -------------------------------------------------

  const EVENT_LABELS = {
    spawn: "启动", crash: "崩溃", rollback: "回滚", stop: "停止",
  };

  function fmtRelative(d) {
    const delta = (d.getTime() - Date.now()) / 1000;
    const abs = Math.abs(delta);
    if (abs < 60)        return rtf.format(Math.round(delta), "second");
    if (abs < 3600)      return rtf.format(Math.round(delta / 60), "minute");
    if (abs < 86400)     return rtf.format(Math.round(delta / 3600), "hour");
    return rtf.format(Math.round(delta / 86400), "day");
  }

  function renderEntry(e) {
    const li = document.createElement("li");
    li.className = `tl tl-${e.event}`;
    const dot = document.createElement("span");
    dot.className = "tl-dot";
    dot.setAttribute("aria-hidden", "true");
    const body = document.createElement("div");
    body.className = "tl-body";

    const head = document.createElement("div");
    head.className = "tl-head";
    const label = document.createElement("span");
    label.className = "tl-label";
    label.textContent = EVENT_LABELS[e.event] || e.event;
    const ver = document.createElement("span");
    ver.className = "mono small tl-version";
    ver.textContent = e.version || "—";
    head.appendChild(label);
    head.appendChild(ver);
    if (e.exit_code !== undefined && e.exit_code !== null) {
      const code = document.createElement("span");
      code.className = "mono small tl-code";
      code.textContent = `exit=${e.exit_code}`;
      head.appendChild(code);
    }

    const meta = document.createElement("div");
    meta.className = "tl-meta";
    const ts = new Date(e.ts);
    const abs = document.createElement("time");
    abs.className = "mono small";
    abs.dateTime = e.ts;
    abs.textContent = tfmt.format(ts);
    const rel = document.createElement("span");
    rel.className = "muted small";
    rel.textContent = fmtRelative(ts);
    meta.appendChild(abs);
    meta.appendChild(document.createTextNode(" · "));
    meta.appendChild(rel);
    if (e.reason) {
      meta.appendChild(document.createTextNode(" · "));
      const reason = document.createElement("span");
      reason.className = "muted small";
      reason.textContent = e.reason;
      meta.appendChild(reason);
    }

    body.appendChild(head);
    body.appendChild(meta);
    li.appendChild(dot);
    li.appendChild(body);
    return li;
  }

  async function loadHistory() {
    try {
      const res = await fetch("/api/history?limit=20");
      if (res.status === 503) {
        historyCard.hidden = true;
        return;
      }
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const payload = await res.json();
      historyCard.hidden = false;
      historyList.replaceChildren();
      const entries = (payload.entries || []).slice().reverse();
      for (const e of entries) historyList.appendChild(renderEntry(e));
      historyEmpty.hidden = entries.length > 0;
      historyHint.innerHTML = `共 <span class="mono small">${payload.count}</span> 条记录 · <span class="mono small">${payload.path}</span>`;
    } catch (err) {
      historyCard.hidden = false;
      historyList.replaceChildren();
      historyEmpty.hidden = false;
      historyEmpty.textContent = `加载失败:${err.message || err}`;
    }
  }

  // --- blacklist -------------------------------------------------------

  function fmtTs(iso) {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    return `${tfmt.format(d)} · ${fmtRelative(d)}`;
  }

  function renderBlacklistEntry(e) {
    const li = document.createElement("li");
    li.className = "bl-item";
    const head = document.createElement("div");
    head.className = "bl-head";
    const ver = document.createElement("span");
    ver.className = "mono bl-version";
    ver.textContent = e.version;
    head.appendChild(ver);
    if (e.version && e.version === currentVersion) {
      const tag = document.createElement("span");
      tag.className = "bl-tag";
      tag.textContent = "当前";
      head.appendChild(tag);
    }
    const meta = document.createElement("div");
    meta.className = "bl-meta mono small";
    meta.textContent = [fmtTs(e.ts), e.reason].filter(Boolean).join(" · ");
    li.appendChild(head);
    li.appendChild(meta);
    return li;
  }

  async function loadBlacklist() {
    try {
      const res = await fetch("/api/blacklist");
      if (res.status === 503) {
        blacklistCard.hidden = true;
        return;
      }
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const payload = await res.json();
      blacklistCard.hidden = false;
      blacklistList.replaceChildren();
      const entries = (payload.entries || []).slice().reverse();
      for (const e of entries) blacklistList.appendChild(renderBlacklistEntry(e));
      blacklistEmpty.hidden = entries.length > 0;
    } catch (err) {
      blacklistCard.hidden = false;
      blacklistEmpty.hidden = false;
      blacklistEmpty.textContent = `加载失败:${err.message || err}`;
    }
  }

  async function markCurrentBad() {
    const ver = currentVersion || "(未知)";
    const ok = window.confirm(
      `确认把版本 ${ver} 标记为坏?\n\n监工(clawmastd)将立即回滚到上一个版本,` +
      `并在后续启动中拒绝再次拉起该版本。`,
    );
    if (!ok) return;
    markBadBtn.dataset.loading = "1";
    markBadBtn.disabled = true;
    try {
      const res = await fetch("/api/blacklist", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ reason: "user-marked-bad" }),
      });
      if (!res.ok && res.status !== 202) {
        throw new Error(`${res.status} ${res.statusText}`);
      }
      const payload = await res.json();
      blacklistResult.hidden = false;
      if (payload.rollback_requested) {
        blacklistResult.textContent =
          `已标记 ${payload.marked_version} 为坏,worker 即将退出(65),监工会翻转符号链接。` +
          `几秒后此页面可能短暂中断,恢复后将运行在上一个版本。`;
        setStatus("error", "即将回滚…");
      } else {
        blacklistResult.textContent = `已标记 ${payload.marked_version} 为坏(未触发回滚)。`;
      }
      await loadBlacklist();
    } catch (err) {
      blacklistResult.hidden = false;
      blacklistResult.textContent = `标记失败:${err.message || err}`;
    } finally {
      markBadBtn.dataset.loading = "0";
      markBadBtn.disabled = false;
    }
  }

  historyRefreshBtn.addEventListener("click", loadHistory);
  checkBtn.addEventListener("click", checkUpdates);
  markBadBtn.addEventListener("click", markCurrentBad);

  loadVersion().then(loadBlacklist);
  poll();
  loadHistory();
  setInterval(poll, 5000);
  setInterval(loadHistory, 15000);
  setInterval(loadBlacklist, 30000);
})();
