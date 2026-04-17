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

  const fmt = new Intl.RelativeTimeFormat("zh-Hans", { numeric: "always" });

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
      versionEl.textContent = v.version || "dev";
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

  checkBtn.addEventListener("click", checkUpdates);

  loadVersion();
  poll();
  setInterval(poll, 5000);
})();
