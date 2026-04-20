// ClawMast walking-skeleton dashboard client.
//
// No framework, no build step — deliberately. The dashboard focuses on
// OpenClaw status + one-click actions; ClawMast's own version/update
// UI lives under Settings.
//
// Later iterations replace this file with a React + Vite bundle; the
// embed FS shape stays the same (architecture/refactor.md §9).

(() => {
  const $ = (id) => document.getElementById(id);

  // Dashboard — status + footer.
  const statusText = $("status-text");
  const footerVersion = $("footer-version");
  const footerUptime = $("footer-uptime");

  // OpenClaw card.
  const openclawCard = $("openclaw-card");
  const openclawSub = $("openclaw-sub");
  const openclawBadge = $("openclaw-badge");
  const openclawBadgeText = $("openclaw-badge-text");
  const openclawChannelsRow = $("openclaw-channels-row");
  const openclawChannels = $("openclaw-channels");
  const openclawAgentRow = $("openclaw-agent-row");
  const openclawAgent = $("openclaw-agent");

  // Structured health rows. Each row carries one signal; the card
  // composes them into the overall 运行中 / 异常 / 已停止 verdict
  // via the badge + button set.
  const openclawRowService = $("openclaw-row-service");
  const openclawServiceText = $("openclaw-service-text");
  const openclawRowPort = $("openclaw-row-port");
  const openclawPortText = $("openclaw-port-text");
  // Gateway diagnostic affordance. The chevron sits in the row; clicking
  // opens #gateway-dialog populated from the latest snapshot. Replaces
  // the prior inline <details>/<pre> dump so the card stays scannable
  // and the raw error lives in a purposeful sub-screen.
  const openclawGatewayMore = $("openclaw-gateway-more");
  const gatewayDialog = $("gateway-dialog");
  const gatewayDialogState = $("gateway-dialog-state");
  const gatewayDialogWhen = $("gateway-dialog-when");
  const gatewayDialogDuration = $("gateway-dialog-duration");
  const gatewayDialogCmd = $("gateway-dialog-cmd");
  const gatewayDialogPID = $("gateway-dialog-pid");
  const gatewayDialogUptime = $("gateway-dialog-uptime");
  const gatewayDialogCrashes = $("gateway-dialog-crashes");
  const gatewayDialogAutohealLabel = $("gateway-dialog-autoheal-label");
  const gatewayDialogAutoheal = $("gateway-dialog-autoheal");
  const gatewayDialogSparkWrap = $("gateway-dialog-spark-wrap");
  const gatewayDialogSpark = $("gateway-dialog-spark");
  const gatewayDialogErrorWrap = $("gateway-dialog-error-wrap");
  const gatewayDialogError = $("gateway-dialog-error");
  const gatewayDialogCopy = $("gateway-dialog-copy");

  // Destructive-action confirmation dialog.
  const confirmDialog = $("confirm-dialog");
  const confirmForm = $("confirm-form");
  const confirmTitle = $("confirm-title");
  const confirmBody = $("confirm-body");
  const confirmCancel = $("confirm-cancel");

  // Persistent bottom console. Shared by every action + fix stream.
  // The status label + outcome dot live in the always-visible head;
  // the log itself only takes space in expanded / running states.
  const consoleEl = $("console");
  const consoleHead = $("console-head");
  const consoleLabel = $("console-label");
  const consoleLog = $("console-log");
  const consoleToggle = $("console-toggle");
  const consoleClear = $("console-clear");

  // Token prompt (legacy dialog — unchanged).
  const tokenDialog = $("token-dialog");
  const tokenForm = $("token-form");
  const tokenInput = $("token-input");
  const tokenError = $("token-error");

  // Settings view.
  const settingsVersion = $("settings-version");
  const settingsBuild = $("settings-build");
  const settingsFull = $("settings-full");
  const settingsLatest = $("settings-latest");
  const settingsUpdateNote = $("settings-update-note");
  const settingsCheck = $("settings-check");
  const settingsInstall = $("settings-install");
  const settingsUpdateDot = $("settings-update-dot");

  // System card (sibling of the version/update card).
  const sysWorker = $("sys-worker");
  const sysWorkerBuild = $("sys-worker-build");
  const sysSupervisor = $("sys-supervisor");
  const sysSupervisorNote = $("sys-supervisor-note");
  const sysInstallRoot = $("sys-install-root");
  const sysUptime = $("sys-uptime");

  // Channel picker (segmented control + confirm dialog).
  const channelButtons = document.querySelectorAll(".seg-btn[data-channel]");
  const channelNote = $("channel-note");
  const channelDialog = $("channel-dialog");
  const channelForm = $("channel-form");
  const channelCancel = $("channel-cancel");

  const rtf = new Intl.RelativeTimeFormat("zh-Hans", { numeric: "auto" });
  const tfmt = new Intl.DateTimeFormat("zh-Hans", {
    hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false,
  });

  // --- bearer token plumbing --------------------------------------------
  //
  // The worker enforces Authorization: Bearer on every /api/** except
  // /api/health when the connection is non-loopback. We keep the token
  // in localStorage so reloads on the same device don't re-prompt; any
  // 401 triggers a modal and retries the original request once.
  const TOKEN_KEY = "clawmast:bearer";
  const getToken = () => {
    try { return window.localStorage.getItem(TOKEN_KEY) || ""; }
    catch { return ""; }
  };
  const setToken = (v) => {
    try { window.localStorage.setItem(TOKEN_KEY, v); }
    catch { /* private mode: keep in-memory only */ }
  };
  const clearToken = () => {
    try { window.localStorage.removeItem(TOKEN_KEY); } catch {}
  };

  function withAuth(init) {
    const headers = new Headers(init && init.headers ? init.headers : undefined);
    const t = getToken();
    if (t) headers.set("Authorization", `Bearer ${t}`);
    return { ...(init || {}), headers };
  }

  // promptForToken shows the modal and resolves with true when the user
  // saves a new token, or false when they cancel. Only one modal is open
  // at a time; concurrent 401s await the same promise.
  let tokenPromise = null;
  function promptForToken(reason) {
    if (tokenPromise) return tokenPromise;
    tokenError.hidden = !reason;
    tokenError.textContent = reason || "";
    tokenInput.value = "";
    if (typeof tokenDialog.showModal === "function") tokenDialog.showModal();
    else tokenDialog.setAttribute("open", "");
    tokenPromise = new Promise((resolve) => {
      const onSubmit = (ev) => {
        ev.preventDefault();
        const v = (tokenInput.value || "").trim();
        if (!v) return;
        setToken(v);
        tokenForm.removeEventListener("submit", onSubmit);
        if (typeof tokenDialog.close === "function") tokenDialog.close();
        else tokenDialog.removeAttribute("open");
        tokenPromise = null;
        resolve(true);
      };
      tokenForm.addEventListener("submit", onSubmit);
    });
    return tokenPromise;
  }

  async function jfetch(url, init) {
    let res = await fetch(url, withAuth(init));
    if (res.status === 401) {
      clearToken();
      const entered = await promptForToken("服务端拒绝,当前 token 无效或已更换。");
      if (!entered) throw new Error("401 unauthorized");
      res = await fetch(url, withAuth(init));
    }
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

  // setStatus updates both the desktop sidebar badge and the mobile
  // header mirror in one pass. The legacy dashboard showed the same
  // pill in two layout slots (sidebar + top bar); we preserve that
  // by mirroring via the data-mirror attribute on the mobile node.
  function setStatus(kind, text) {
    const badges = document.querySelectorAll('#status-badge, #status-badge-mobile');
    badges.forEach((el) => {
      el.classList.remove("badge-unknown", "badge-healthy", "badge-error");
      el.classList.add(`badge-${kind}`);
    });
    statusText.textContent = text;
    document.querySelectorAll('[data-mirror="status-text"]').forEach((el) => {
      el.textContent = text;
    });
  }

  // setOpenclawSub writes the one-line subtitle under the object
  // title in the card header. Keeps the status story in a single
  // spot — the badge carries the tone; the sub carries the detail.
  function setOpenclawSub(text) {
    openclawSub.textContent = text || "";
  }

  async function loadVersion() {
    try {
      const v = await jfetch("/api/version");
      // Prefer the supervisor-assigned label; fall back to the ldflags
      // version in standalone mode.
      const label = v.label || v.version || "dev";
      footerVersion.textContent = label;
      settingsVersion.textContent = label;
      settingsBuild.textContent = `${v.commit || "none"} · ${v.build_time || "unknown"}`;
      settingsFull.textContent = v.full || "";

      // System card.
      sysWorker.textContent = label;
      sysWorkerBuild.textContent = v.full || "";
      if (v.supervisor_version) {
        sysSupervisor.textContent = v.supervisor_version.split(" ")[0] || v.supervisor_version;
        sysSupervisorNote.textContent = v.supervisor_version;
        sysSupervisorNote.classList.remove("warn");
      } else {
        sysSupervisor.textContent = "未检测到";
        sysSupervisorNote.textContent = "worker 以独立模式运行 — 没有 clawmastd 在监管崩溃与自动更新。";
      }
      sysInstallRoot.textContent = v.install_root || "(独立模式)";
    } catch (err) {
      footerVersion.textContent = "加载失败";
      settingsVersion.textContent = "加载失败";
      settingsBuild.textContent = String(err.message || err);
      sysWorker.textContent = "加载失败";
    }
  }

  async function poll() {
    try {
      const h = await jfetch("/api/health");
      setStatus(h.ok ? "healthy" : "error", h.ok ? "运行中" : "异常");
      footerUptime.textContent = `uptime ${fmtUptime(h.uptime_ms)}`;
      if (sysUptime) sysUptime.textContent = fmtUptime(h.uptime_ms);
    } catch (err) {
      setStatus("error", "无法连接");
      footerUptime.textContent = "—";
      if (sysUptime) sysUptime.textContent = "—";
    }
  }

  // --- openclaw status card --------------------------------------------

  function fmtRelative(d) {
    const delta = (d.getTime() - Date.now()) / 1000;
    const abs = Math.abs(delta);
    if (abs < 60)    return rtf.format(Math.round(delta), "second");
    if (abs < 3600)  return rtf.format(Math.round(delta / 60), "minute");
    if (abs < 86400) return rtf.format(Math.round(delta / 3600), "hour");
    return rtf.format(Math.round(delta / 86400), "day");
  }

  function relStamp(iso) {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    return `${tfmt.format(d)} · ${fmtRelative(d)}`;
  }

  function setOpenclawTone(tone) {
    openclawCard.classList.remove("tone-down", "tone-warn", "tone-info");
    if (tone) openclawCard.classList.add(`tone-${tone}`);
  }

  function setOpenclawBadge(kind, text) {
    openclawBadge.classList.remove(
      "badge-unknown",
      "badge-healthy",
      "badge-error",
      "badge-pending",
      "badge-starting",
    );
    openclawBadge.classList.add(`badge-${kind}`);
    openclawBadgeText.textContent = text;
  }

  // Drive the action grid off the probed state. Every button is
  // always visible so the layout is stable across state changes; only
  // the disabled flag toggles. This also self-documents — a greyed-
  // out 重启 in the 已停止 state tells the operator "restart doesn't
  // apply here" without them having to remember which buttons belong
  // to which state.
  //
  //                   一键修复   启动   停止   重启
  //   运行中            ✓         ·     ✓      ✓
  //   异常 (offline)    ✓         ✓     ✓      ·
  //   已停止             ✓         ✓     ·      ·
  //   未安装 / 检测中     ·         ·     ·      ·
  //
  // 一键修复 is state-independent because it repairs config / service
  // / env — useful regardless of live state. 启动/停止/重启 are
  // state-dependent: each only enables when its target transition
  // makes sense.
  //
  // runAction() disables every button for the duration of a request
  // and calls pollOpenClaw() at the end, which re-enters this
  // function to restore the right set.
  function setActionState(mode) {
    const btns = {
      fix:     document.querySelector('[data-action="fix"]'),
      start:   document.querySelector('[data-action="start"]'),
      stop:    document.querySelector('[data-action="stop"]'),
      restart: document.querySelector('[data-action="restart"]'),
    };
    // Always visible — only disabled state changes.
    for (const b of Object.values(btns)) b.hidden = false;
    if (mode === "online") {
      btns.fix.disabled = false;
      btns.start.disabled = true;
      btns.restart.disabled = false;
      btns.stop.disabled = false;
    } else if (mode === "offline") {
      btns.fix.disabled = false;
      btns.start.disabled = false;
      btns.restart.disabled = true;
      btns.stop.disabled = false;
    } else if (mode === "stopped") {
      btns.fix.disabled = false;
      btns.start.disabled = false;
      btns.restart.disabled = true;
      btns.stop.disabled = true;
    } else { // "unknown" or "missing"
      btns.fix.disabled = true;
      btns.start.disabled = true;
      btns.restart.disabled = true;
      btns.stop.disabled = true;
    }
  }

  // setHealthRow writes a row's icon state ("ok"/"err"/"warn"/"off"/
  // "unknown") + text in one call. The row element also carries the
  // state via data-state so CSS can tint the text.
  function setHealthRow(row, textEl, state, text) {
    if (!row || !textEl) return;
    row.dataset.state = state;
    const icon = row.querySelector(".health-icon");
    if (icon) icon.dataset.state = state;
    textEl.textContent = text;
  }

  // setGatewayRow is the specialised setter for the Gateway row. The
  // row displays a short verdict word ("运行中" / "已停止" / "未响应" …).
  // The chevron visibility and dialog payload are driven separately by
  // setGatewayDiag(snapshot) so the dialog can always surface PID /
  // uptime / crash count / probe history, not just error details.
  function setGatewayRow(state, text) {
    setHealthRow(openclawRowPort, openclawPortText, state, text);
  }

  // gatewayDiag is the data bag the dialog reads when the chevron is
  // clicked. Populated by setGatewayDiag from the full snapshot so
  // every render refreshes observability numbers, not only the
  // abnormal path. Null clears the chevron (used when we have no
  // useful info — pre-probe, CLI missing, or deliberately stopped).
  let gatewayDiag = null;
  function setGatewayDiag(s, stateText, errText) {
    if (!openclawGatewayMore) return;
    if (!s) {
      gatewayDiag = null;
      openclawGatewayMore.hidden = true;
      return;
    }
    gatewayDiag = {
      stateText: stateText || "",
      probeAtISO: s.last_probe_at || "",
      probeMS: s.last_probe_ms || 0,
      probeURL: gatewayHealthURL(s),
      pid: s.gateway_pid || 0,
      pidSinceISO: s.gateway_pid_since || "",
      crashCount: s.crash_count || 0,
      history: Array.isArray(s.probe_history) ? s.probe_history : [],
      autohealEnabled: !!s.autoheal_enabled,
      autohealCount: s.autoheal_count || 0,
      autohealLastAtISO: s.autoheal_last_at || "",
      autohealNextEligibleAtISO: s.autoheal_next_eligible_at || "",
      error: (errText || "").trim(),
    };
    openclawGatewayMore.hidden = false;
  }

  // gatewayEndpoint renders the "host:port" pair the probe actually
  // uses, sourced from the snapshot's resolved address cache. Falls
  // back to the documented default only when the backend resolver has
  // not replied yet (first ~1 s after boot) — the seed the backend
  // ships matches the fallback so the UI never reads "undefined:NaN".
  function gatewayEndpoint(s) {
    const host = (s && s.gateway_host) || "127.0.0.1";
    const port = (s && s.gateway_port) || 18789;
    return `${host}:${port}`;
  }
  function gatewayHealthURL(s) {
    return `http://${gatewayEndpoint(s)}/health`;
  }

  // applyPendingState paints the optimistic "action in flight" look on
  // the card the instant a button is clicked. The backend will catch
  // up within 1–2 probes (the next ProbeNow sees currentAction set by
  // RunAction and computes PhaseStarting), but rendering immediately
  // avoids the ~500 ms window where the UI still shows 运行中 while
  // the CLI spawn is already underway. Badge is blue/breathing for
  // start/restart/fix and for stop — all four read as "busy" rather
  // than "problem". The pending state is cleared when renderOpenClaw
  // runs with activeAction=null after the action stream closes.
  function applyPendingState(name) {
    const p = ACTION_PENDING[name];
    if (!p) return;
    setOpenclawBadge("starting", p.badge);
    setOpenclawTone("info");
    setOpenclawSub(p.sub);
    setGatewayRow("starting", p.row);
  }

  // renderOpenClaw drives every visual on the OpenClaw card from the
  // single Phase field on the Snapshot. The backend is the source of
  // truth: no client-side re-derivation from alive/intent/cli_missing
  // combinations (that two-sided decision table is what flapped the
  // badge through 异常 during warmup before this refactor). Each phase
  // branch owns its badge, tone, sub, Gateway row, and diag dialog —
  // kept exhaustive and parallel so adding a new phase means adding
  // exactly one case, not patching N call sites.
  function renderOpenClaw(s) {
    // Freeze the card while an action is streaming. The 1 s poller
    // would otherwise race with the optimistic paint from
    // applyPendingState during the first probe after the click.
    if (activeAction) return;

    const phase = s.phase || "";

    // CLI row (shared across every non-missing phase): surface the
    // resolved binary path + discovery source so operators can tell
    // whether brew, an env override, or state/openclaw-path is
    // winning. Painted before the switch because every alive-capable
    // phase wants it; the missing branch overrides below.
    if (phase !== "" && phase !== "missing") {
      const cliBits = [];
      if (s.binary_path) cliBits.push(s.binary_path);
      if (s.binary_source) cliBits.push(`(${s.binary_source})`);
      setHealthRow(openclawRowService, openclawServiceText, "ok",
        cliBits.join(" ") || "已发现 openclaw");
    }

    switch (phase) {
      case "": // PhaseUnknown — pre-probe.
        setOpenclawBadge("unknown", "检测中");
        setActionState("unknown");
        setOpenclawTone(null);
        setOpenclawSub("连接本机 openclaw CLI 并探测网关状态");
        setHealthRow(openclawRowService, openclawServiceText, "unknown", "检测中");
        setGatewayRow("unknown", "检测中");
        setGatewayDiag(null);
        return;

      case "missing":
        setOpenclawBadge("error", "未安装");
        setActionState("missing");
        setOpenclawTone("down");
        openclawChannelsRow.hidden = true;
        openclawAgentRow.hidden = true;
        setOpenclawSub("未安装 openclaw CLI · 请按官方文档完成安装");
        setHealthRow(openclawRowService, openclawServiceText, "err", "openclaw CLI 未安装");
        setGatewayRow("off", "—");
        setGatewayDiag(null);
        return;

      case "stopped":
        setOpenclawBadge("unknown", "已停止");
        setActionState("stopped");
        setOpenclawTone(null);
        setOpenclawSub("已手动停止 · 点击启动重新拉起");
        setGatewayRow("off", "已停止");
        setGatewayDiag(s, "已停止", "");
        break;

      case "starting":
        // Backend is either (a) mid-action via currentAction or (b)
        // inside the 15 s warmup-grace window after a successful
        // start/restart/fix CLI return. Either way the operator's
        // mental model is "it's coming up" — blue, breathing, no
        // error. Even a page refresh during warmup keeps this state
        // because lastStartAttemptAt lives on the Manager.
        setOpenclawBadge("starting", "启动中");
        setActionState("offline");
        setOpenclawTone("info");
        setOpenclawSub("正在启动 OpenClaw…");
        setGatewayRow("starting", "启动中…");
        setGatewayDiag(s, "启动中", "");
        break;

      case "running":
        setOpenclawBadge("healthy", "运行中");
        setActionState("online");
        setOpenclawTone(null);
        {
          const bits = [];
          bits.push(s.version ? `v${s.version}` : "OpenClaw");
          bits.push(gatewayEndpoint(s));
          setOpenclawSub(bits.join(" · "));
        }
        setGatewayRow("ok", "运行中");
        setGatewayDiag(s, "运行中", "");
        break;

      case "error":
      default:
        // Genuine abnormal state: !alive with no intent=stopped cover
        // and no warmup grace left. probe_error goes in the diag
        // dialog (chevron) so the card surface stays single-word.
        setOpenclawBadge("error", "异常");
        setActionState("offline");
        setOpenclawTone("down");
        setOpenclawSub("点击一键修复,或使用启动按钮手动拉起");
        setGatewayRow("err", "未响应");
        setGatewayDiag(s, "未响应", s.probe_error || "");
        break;
    }
    lastSessionsCount = s.sessions_count || 0;
    openclawChannelsRow.hidden = !(s.channel_count || s.sessions_count);
    openclawChannels.textContent = `${s.channel_count || 0} 个 · ${s.sessions_count || 0} 会话`;
    openclawAgentRow.hidden = !s.default_agent_id;
    openclawAgent.textContent = s.default_agent_id || "—";
  }

  async function pollOpenClaw() {
    try {
      const res = await fetch("/api/openclaw/status", withAuth());
      if (res.status === 503) { openclawCard.hidden = true; return; }
      if (res.status === 401) {
        await promptForToken("需要 bearer token 才能读取 openclaw 状态。");
        return;
      }
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const s = await res.json();
      renderOpenClaw(s);
    } catch (err) {
      setOpenclawBadge("error", "无法连接");
      setActionState("unknown");
      setOpenclawTone("down");
      setOpenclawSub(`探测失败:${err.message || err}`);
    }
  }

  // --- action + bottom console ---------------------------------------
  //
  // The four small action buttons (start/stop/restart/doctor) and the
  // orange 一键修复 button all funnel through runAction(). Fix hits
  // /api/openclaw/fix which streams a multi-step cascade; the others hit
  // /api/openclaw/action?name=… which emits exactly one start+end pair.
  // Both flows feed the persistent bottom console (#console) so the
  // dashboard layout never shifts when output arrives.

  const ACTION_LABELS = {
    fix: "一键修复", start: "启动", stop: "停止", restart: "重启",
  };

  // ACTION_PENDING drives the optimistic "in-flight" treatment that
  // paints the card the instant a button is clicked — we do not wait
  // for the backend probe to catch up. Without this, restart goes
  // through a ~6 s window where the UI still says 运行中 until the
  // gateway drops, then flaps to 异常, then back to 运行中 once the
  // post-action probe confirms. With this, the card goes straight to
  // 重启中 (blue, breathing dot via applyPendingState) and the card is
  // frozen until runAction's finally block clears activeAction — then
  // the phase-driven renderOpenClaw takes over, which will still read
  // PhaseStarting from the backend for the remainder of the warmup.
  const ACTION_PENDING = {
    start:   { badge: "启动中",   sub: "正在启动 OpenClaw…",   row: "启动中…" },
    stop:    { badge: "停止中",   sub: "正在停止 OpenClaw…",   row: "停止中…" },
    restart: { badge: "重启中",   sub: "正在重启 OpenClaw…",   row: "重启中…" },
    fix:     { badge: "修复中",   sub: "正在执行一键修复…",   row: "修复中…" },
  };

  // activeAction holds the name of the action currently streaming. When
  // non-null, renderOpenClaw() early-returns — the pending UI stays
  // put even as the 1 s status poll keeps running in the background.
  // runAction() clears it in `finally` and then calls pollOpenClaw()
  // to paint the post-action reality.
  let activeAction = null;

  // Actions that interrupt active sessions prompt first. The copy
  // adapts to the current sessions_count so "1 个会话" vs "当前没有
  // 活跃会话" reads naturally.
  const CONFIRM_COPY = {
    fix: {
      title: "运行一键修复?",
      body: "一键修复会依次检查环境、重装服务、重启 gateway,最长约 30 秒。",
    },
    restart: {
      title: "重启 OpenClaw Gateway?",
      body: "重启会关闭并重新拉起 gateway,大约持续 5 秒。",
    },
  };

  // confirmAction shows the reusable modal and resolves true if the
  // user hit 继续, false otherwise (cancel / dismiss / Esc). Only one
  // dialog is open at a time — the browser enforces this on <dialog>.
  function confirmAction(name, sessionsCount) {
    const copy = CONFIRM_COPY[name];
    if (!copy) return Promise.resolve(true);
    confirmTitle.textContent = copy.title;
    const sess = Number.isFinite(sessionsCount) && sessionsCount > 0
      ? `当前 ${sessionsCount} 个活跃会话会被中断。`
      : "当前没有活跃会话,可以安全继续。";
    confirmBody.textContent = `${copy.body} ${sess}`;
    return new Promise((resolve) => {
      const onSubmit = (ev) => {
        ev.preventDefault();
        cleanup();
        confirmDialog.close();
        resolve(true);
      };
      const onCancel = () => { cleanup(); confirmDialog.close(); resolve(false); };
      const onEsc = (ev) => {
        // <dialog> fires a 'cancel' event on Esc; listen on the
        // dialog itself rather than the form so dismissal works even
        // without a button click.
        ev.preventDefault();
        cleanup();
        confirmDialog.close();
        resolve(false);
      };
      function cleanup() {
        confirmForm.removeEventListener("submit", onSubmit);
        confirmCancel.removeEventListener("click", onCancel);
        confirmDialog.removeEventListener("cancel", onEsc);
      }
      confirmForm.addEventListener("submit", onSubmit);
      confirmCancel.addEventListener("click", onCancel);
      confirmDialog.addEventListener("cancel", onEsc);
      if (typeof confirmDialog.showModal === "function") confirmDialog.showModal();
      else confirmDialog.setAttribute("open", "");
    });
  }

  // Console has three states: collapsed (thin bar), expanded (user
  // opened), running (auto-expanded during an in-flight action). The
  // last-outcome class survives across state changes so the dot keeps
  // its green/red tint after the action finishes.
  function setConsoleState(state) {
    consoleEl.dataset.state = state;
  }
  function setConsoleLabel(text) {
    consoleLabel.textContent = text || "控制台 · 等待操作";
  }
  function setConsoleOutcome(outcome) {
    consoleEl.classList.remove("outcome-ok", "outcome-err");
    if (outcome === "ok") consoleEl.classList.add("outcome-ok");
    else if (outcome) consoleEl.classList.add("outcome-err");
  }
  // has-log toggles the Clear button and any other "there is content"
  // affordances so the idle console stays visually quiet.
  function setConsoleHasLog(has) {
    consoleEl.classList.toggle("has-log", !!has);
  }

  function appendConsoleLine(text, kind) {
    if (!text) return;
    const span = document.createElement("span");
    span.className = kind ? `ln-${kind}` : "";
    span.textContent = text.endsWith("\n") ? text : text + "\n";
    consoleLog.appendChild(span);
    consoleLog.scrollTop = consoleLog.scrollHeight;
    setConsoleHasLog(true);
  }

  function applyStreamEvent(ev) {
    if (ev.phase === "start") {
      if (ev.command) appendConsoleLine(`$ ${ev.command}`, "sys");
      return;
    }
    // end phase: append stdout/stderr tails then a summary line.
    if (ev.stdout_tail) appendConsoleLine(ev.stdout_tail);
    if (ev.stderr_tail) appendConsoleLine(ev.stderr_tail, "err");
    if (ev.note) appendConsoleLine(ev.note, "err");
    const outcome = ev.outcome || "failed";
    const ms = ev.duration_ms ? ` · ${ev.duration_ms} ms` : "";
    const code = (typeof ev.exit_code === "number" && ev.exit_code !== 0)
      ? ` · exit=${ev.exit_code}` : "";
    appendConsoleLine(`[${outcome}${ms}${code}]`, "sys");
  }

  // Header is the toggle surface; buttons inside stop propagation so
  // clicking Clear / chevron doesn't double-fire. We avoid toggling
  // while an action is running — the running drawer stays up until
  // the request settles so users don't lose output mid-stream.
  consoleHead.addEventListener("click", (ev) => {
    if (ev.target.closest(".console-btn")) return;
    if (consoleEl.dataset.state === "running") return;
    setConsoleState(consoleEl.dataset.state === "collapsed" ? "expanded" : "collapsed");
  });
  consoleToggle.addEventListener("click", (ev) => {
    ev.stopPropagation();
    if (consoleEl.dataset.state === "running") return;
    setConsoleState(consoleEl.dataset.state === "collapsed" ? "expanded" : "collapsed");
  });
  consoleClear.addEventListener("click", (ev) => {
    ev.stopPropagation();
    consoleLog.replaceChildren();
    setConsoleOutcome(null);
    setConsoleLabel("控制台 · 等待操作");
    setConsoleHasLog(false);
    setConsoleState("collapsed");
  });

  // Buttons are indexed once at boot; we toggle their disabled state from
  // renderOpenClaw() rather than binding+rebinding click handlers.
  const actionButtons = Array.from(document.querySelectorAll('[data-action]'));
  for (const btn of actionButtons) {
    btn.addEventListener("click", () => runAction(btn.dataset.action));
  }

  // Gateway diagnostic dialog. The chevron on the Gateway row opens it;
  // Esc and backdrop close via native <dialog> semantics, and the form's
  // submit button ("关闭") does the same without needing a JS handler.
  // Copy button serialises the rendered key/values + raw error so the
  // operator can paste them into a bug report.
  function openGatewayDialog() {
    if (!gatewayDialog || !gatewayDiag) return;
    gatewayDialogState.textContent = gatewayDiag.stateText || "—";
    gatewayDialogWhen.textContent = gatewayDiag.probeAtISO
      ? relStamp(gatewayDiag.probeAtISO)
      : "—";
    gatewayDialogDuration.textContent = gatewayDiag.probeMS
      ? `${gatewayDiag.probeMS} ms`
      : "—";
    // Show the actual HTTP URL we GET for liveness. The probe runs
    // against the resolver-tracked host:port, so an operator who
    // bumped the port in openclaw.json sees the new URL here within
    // one address-refresh cycle rather than a stale hardcoded string.
    if (gatewayDialogCmd) {
      gatewayDialogCmd.textContent = gatewayDiag.probeURL
        ? `GET ${gatewayDiag.probeURL}`
        : "GET http://127.0.0.1:18789/health";
    }
    if (gatewayDialogPID) {
      gatewayDialogPID.textContent = gatewayDiag.pid
        ? String(gatewayDiag.pid)
        : "—";
    }
    if (gatewayDialogUptime) {
      gatewayDialogUptime.textContent = gatewayDiag.pidSinceISO
        ? relStamp(gatewayDiag.pidSinceISO)
        : "—";
    }
    if (gatewayDialogCrashes) {
      gatewayDialogCrashes.textContent = String(gatewayDiag.crashCount || 0);
    }
    // Autoheal row: visible only when the operator opted in. The three
    // copy branches are "待命" (enabled, never fired), "冷却中 · 剩 N"
    // (enabled, within cooldown window), and "就绪 · 上次 …" (enabled,
    // past the cooldown). formatAutoheal centralises the logic so the
    // Copy-to-clipboard path picks up the same rendering.
    if (gatewayDialogAutohealLabel && gatewayDialogAutoheal) {
      const hide = !gatewayDiag.autohealEnabled;
      gatewayDialogAutohealLabel.hidden = hide;
      gatewayDialogAutoheal.hidden = hide;
      if (!hide) {
        gatewayDialogAutoheal.textContent = formatAutoheal(gatewayDiag);
      }
    }
    renderSparkline(gatewayDiag.history);
    const err = gatewayDiag.error || "";
    gatewayDialogErrorWrap.hidden = !err;
    gatewayDialogError.textContent = err;
    if (typeof gatewayDialog.showModal === "function") gatewayDialog.showModal();
    else gatewayDialog.setAttribute("open", "");
  }

  // formatAutoheal renders the dialog's 自愈 row based on the three
  // observable states of the opt-in policy:
  //
  //   never fired  → "已启用 · 待命"
  //   in cooldown  → "冷却中 · 剩 Xm · 已触发 N 次"
  //   past cooldown→ "就绪 · 上次 Xm 前 · 已触发 N 次"
  //
  // All three branches are rendered on one line so the dt/dd grid
  // stays visually aligned with the rows above (PID, uptime, crashes).
  function formatAutoheal(d) {
    const count = d.autohealCount || 0;
    if (count === 0) return "已启用 · 待命";
    const now = Date.now();
    const nextMs = d.autohealNextEligibleAtISO
      ? new Date(d.autohealNextEligibleAtISO).getTime()
      : 0;
    const lastFmt = d.autohealLastAtISO ? fmtRelative(new Date(d.autohealLastAtISO)) : "—";
    if (Number.isFinite(nextMs) && nextMs > now) {
      const remain = humanizeMS(nextMs - now);
      return `冷却中 · 剩 ${remain} · 已触发 ${count} 次`;
    }
    return `就绪 · 上次 ${lastFmt} · 已触发 ${count} 次`;
  }

  // humanizeMS reduces a millisecond delta to a compact "1h 3m" style
  // string. Chosen over Intl.RelativeTimeFormat because that one only
  // emits a single unit and we need minutes resolution even when the
  // cooldown happens to be 63 minutes.
  function humanizeMS(ms) {
    const s = Math.max(0, Math.round(ms / 1000));
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    const rs = s % 60;
    if (m < 60) return rs ? `${m}m ${rs}s` : `${m}m`;
    const h = Math.floor(m / 60);
    const rm = m % 60;
    return rm ? `${h}h ${rm}m` : `${h}h`;
  }

  // renderSparkline paints the last N probe samples into the dialog's
  // SVG. X = sample index (0..N-1), Y = probe_ms normalised into the
  // 40px container. alive=false samples get a red dot marker on the
  // baseline so flapping is visually distinct from slow probes. We
  // bail out under 2 samples — a single data point draws as a dot
  // nobody can read as a trend.
  function renderSparkline(history) {
    if (!gatewayDialogSpark || !gatewayDialogSparkWrap) return;
    const samples = Array.isArray(history) ? history : [];
    if (samples.length < 2) {
      gatewayDialogSparkWrap.hidden = true;
      gatewayDialogSpark.replaceChildren();
      return;
    }
    gatewayDialogSparkWrap.hidden = false;
    const W = 200, H = 40, padX = 2, padY = 4;
    const inner = W - 2 * padX;
    const stepX = samples.length > 1 ? inner / (samples.length - 1) : 0;
    const okMs = samples.filter(s => s.alive).map(s => s.probe_ms || 0);
    const maxMs = okMs.length ? Math.max(...okMs) : 1;
    const scaleY = (ms) => {
      const clamped = Math.min(Math.max(ms, 0), maxMs || 1);
      return padY + (H - 2 * padY) * (1 - clamped / (maxMs || 1));
    };
    // Line runs through alive samples only — a !alive tick leaves a
    // gap, which reads as "the service was down here".
    const segments = [];
    let current = [];
    samples.forEach((s, i) => {
      const x = padX + i * stepX;
      if (s.alive) {
        current.push(`${x.toFixed(1)},${scaleY(s.probe_ms || 0).toFixed(1)}`);
      } else if (current.length) {
        segments.push(current);
        current = [];
      }
    });
    if (current.length) segments.push(current);

    const parts = [];
    for (const seg of segments) {
      if (seg.length >= 2) {
        parts.push(`<polyline class="line" points="${seg.join(" ")}" />`);
      }
    }
    samples.forEach((s, i) => {
      if (s.alive) return;
      const x = (padX + i * stepX).toFixed(1);
      const y = (H - padY).toFixed(1);
      parts.push(`<circle class="dot fail" cx="${x}" cy="${y}" r="2" />`);
    });
    // Innermost newest sample gets a small dot so "now" is legible
    // even when the line's last vertex is at the right edge.
    const last = samples[samples.length - 1];
    if (last && last.alive) {
      const x = (padX + (samples.length - 1) * stepX).toFixed(1);
      const y = scaleY(last.probe_ms || 0).toFixed(1);
      parts.push(`<circle class="dot" cx="${x}" cy="${y}" r="1.6" />`);
    }
    gatewayDialogSpark.innerHTML = parts.join("");
  }
  if (openclawGatewayMore) {
    openclawGatewayMore.addEventListener("click", openGatewayDialog);
  }
  if (gatewayDialogCopy) {
    gatewayDialogCopy.addEventListener("click", async (ev) => {
      ev.preventDefault();
      const lines = [
        `状态: ${gatewayDialogState.textContent}`,
        `最后探测: ${gatewayDialogWhen.textContent}`,
        `探测用时: ${gatewayDialogDuration.textContent}`,
        `探测命令: ${gatewayDialogCmd.textContent}`,
        `Gateway PID: ${gatewayDialogPID ? gatewayDialogPID.textContent : "—"}`,
        `观察到运行: ${gatewayDialogUptime ? gatewayDialogUptime.textContent : "—"}`,
        `崩溃次数: ${gatewayDialogCrashes ? gatewayDialogCrashes.textContent : "0"}`,
      ];
      // Only surface the 自愈 line when the operator opted in — the
      // dt/dd pair is hidden otherwise and pasting "自愈: —" into a
      // bug report would be pure noise.
      if (gatewayDialogAutohealLabel && !gatewayDialogAutohealLabel.hidden) {
        lines.push(`自愈: ${gatewayDialogAutoheal.textContent}`);
      }
      const err = gatewayDialogError.textContent.trim();
      if (err) lines.push("", "错误信息:", err);
      const payload = lines.join("\n");
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          await navigator.clipboard.writeText(payload);
        } else {
          const ta = document.createElement("textarea");
          ta.value = payload;
          ta.style.position = "fixed";
          ta.style.opacity = "0";
          document.body.appendChild(ta);
          ta.select();
          document.execCommand("copy");
          ta.remove();
        }
        const prev = gatewayDialogCopy.textContent;
        gatewayDialogCopy.textContent = "已复制";
        setTimeout(() => { gatewayDialogCopy.textContent = prev; }, 1200);
      } catch { /* clipboard denied; leave the label untouched */ }
    });
  }

  // Track the last rendered sessions_count so confirm copy reflects
  // the freshest known state without a synchronous probe at click time.
  let lastSessionsCount = 0;

  async function runAction(name) {
    if (!ACTION_LABELS[name]) return;
    const btn = document.querySelector(`[data-action="${name}"]`);
    if (!btn || btn.disabled) return;

    // Destructive actions get a single confirmation prompt. We ask
    // before painting the running state so a cancel leaves the UI
    // untouched.
    if (CONFIRM_COPY[name]) {
      const ok = await confirmAction(name, lastSessionsCount);
      if (!ok) return;
    }

    // Expand the in-flow console for the duration of the action. The
    // panel is always in the DOM — we just flip data-state to make the
    // log region visible and paint the "running" treatment.
    consoleLog.replaceChildren();
    setConsoleOutcome(null);
    setConsoleHasLog(false);
    setConsoleLabel(`${ACTION_LABELS[name]} · 运行中…`);
    setConsoleState("running");

    // Disable every action button while one is in-flight so users can't
    // stack start+stop races. We restore them from renderOpenClaw after
    // the post-action probe refreshes the gateway state.
    for (const b of actionButtons) b.disabled = true;
    btn.dataset.loading = "1";

    // Optimistic pending paint. Set activeAction before applying so
    // renderOpenClaw (which may be mid-fetch from a concurrent poll
    // tick) is frozen by the time the pending frame lands — no flicker
    // between "运行中" and "重启中".
    activeAction = name;
    applyPendingState(name);

    const url = name === "fix"
      ? "/api/openclaw/fix"
      : `/api/openclaw/action?name=${encodeURIComponent(name)}`;
    let finalOutcome = "ok";
    try {
      const res = await fetch(url, withAuth({ method: "POST" }));
      if (res.status === 401) {
        clearToken();
        await promptForToken(`需要 bearer token 才能执行 ${ACTION_LABELS[name]}。`);
        throw new Error("unauthorized");
      }
      if (!res.ok || !res.body) throw new Error(`${res.status} ${res.statusText}`);
      await consumeNDJSON(res.body, (ev) => {
        applyStreamEvent(ev);
        if (ev.phase === "end" && ev.outcome && ev.outcome !== "ok") {
          finalOutcome = ev.outcome;
        }
      });
    } catch (err) {
      appendConsoleLine(`请求失败:${err.message || err}`, "err");
      finalOutcome = "error";
    } finally {
      btn.dataset.loading = "0";
      setConsoleOutcome(finalOutcome === "ok" ? "ok" : "err");
      setConsoleLabel(
        finalOutcome === "ok"
          ? `${ACTION_LABELS[name]} · 完成`
          : `${ACTION_LABELS[name]} · 失败`,
      );
      // Leave the drawer expanded on completion so the user can scan
      // the final output; they collapse it themselves when done.
      setConsoleState("expanded");
      // Clear the pending gate BEFORE pollOpenClaw so the next snapshot
      // re-paints the card with real probe state (alive / probe_error
      // etc). Order matters: flipping activeAction after pollOpenClaw
      // would leave the last pending frame visible until the next 1 s
      // poll tick.
      activeAction = null;
      // ProbeNow on the backend keeps the status poll cheap; we still
      // kick one off here so the button row and sub update immediately
      // after stream close.
      pollOpenClaw();
    }
  }

  // consumeNDJSON reads a ReadableStream and invokes onEvent for each
  // newline-delimited JSON object. Partial lines across chunk boundaries
  // are buffered. Malformed lines are logged (console.warn) and skipped;
  // we must not throw because the backend still owes us a "final" event.
  async function consumeNDJSON(body, onEvent) {
    const reader = body.getReader();
    const decoder = new TextDecoder("utf-8");
    let buf = "";
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try { onEvent(JSON.parse(line)); }
        catch (e) { console.warn("clawmast: bad ndjson line", line, e); }
      }
    }
    const tail = buf.trim();
    if (tail) {
      try { onEvent(JSON.parse(tail)); }
      catch (e) { console.warn("clawmast: bad ndjson tail", tail, e); }
    }
  }

  // --- Settings · ClawMast update check -------------------------------
  //
  // The update UI moved out of the dashboard so the console page is
  // focused on OpenClaw. The nav-dot next to 设置 surfaces a new
  // release without forcing the user to navigate.

  settingsCheck.addEventListener("click", runSettingsCheck);
  settingsInstall.addEventListener("click", runSettingsInstall);

  async function runSettingsCheck() {
    settingsCheck.dataset.loading = "1";
    settingsCheck.disabled = true;
    try {
      const u = await jfetch("/api/updates/check", { method: "POST" });
      renderSettingsUpdate(u);
    } catch (err) {
      settingsUpdateNote.textContent = `请求失败:${err.message || err}`;
      settingsInstall.hidden = true;
    } finally {
      settingsCheck.dataset.loading = "0";
      settingsCheck.disabled = false;
    }
  }

  function renderSettingsUpdate(u) {
    if (u.channel) setChannelActive(u.channel);
    let note = "";
    let canInstall = false;
    if (u.source === "signed-manifest") {
      settingsLatest.textContent = u.latest || "—";
      if (u.update_available) {
        note = `发现新版本 ${u.latest}(当前 ${u.current})`;
        if (u.published_at) note += ` · 发布于 ${u.published_at}`;
        canInstall = true;
      } else {
        note = `已是最新版本(${u.current})`;
      }
      if (u.notes) note += ` · ${u.notes}`;
    } else if (u.source === "not-configured") {
      settingsLatest.textContent = "未配置";
      note = u.note || "未配置更新通道(CLAWMAST_UPDATE_URL)";
    } else if (u.source === "error") {
      settingsLatest.textContent = "检查失败";
      note = u.note || `错误:${u.error_code || "未知"}`;
    } else {
      settingsLatest.textContent = u.latest || "—";
      note = u.note || u.source || "";
    }
    settingsUpdateNote.textContent = note;
    settingsInstall.hidden = !canInstall;
    settingsInstall.disabled = !canInstall;
    settingsInstall.dataset.version = u.latest || "";
    settingsUpdateDot.hidden = !canInstall;
  }

  async function runSettingsInstall() {
    const targetVersion = settingsInstall.dataset.version || "";
    const ok = window.confirm(
      `确认下载并安装 ${targetVersion || "最新版本"}?\n\n` +
      `将校验签名与哈希,写入 versions/ 目录并旋转 current/previous 符号链接,` +
      `然后 worker 会平稳退出;监工(clawmastd)会在新链接下重新拉起。`,
    );
    if (!ok) return;
    settingsInstall.dataset.loading = "1";
    settingsInstall.disabled = true;
    settingsCheck.disabled = true;
    try {
      const res = await fetch("/api/updates/install", withAuth({ method: "POST" }));
      const payload = await res.json();
      if (!res.ok) {
        settingsUpdateNote.textContent = `安装失败:${payload.note || payload.error_code || res.status}`;
        return;
      }
      settingsUpdateNote.textContent =
        `已安装 ${payload.version}(原 ${payload.previous_version || "?"});worker 即将重启。`;
      setStatus("error", "即将重启…");
      settingsInstall.hidden = true;
      settingsUpdateDot.hidden = true;
    } catch (err) {
      settingsUpdateNote.textContent = `请求失败:${err.message || err}`;
    } finally {
      settingsInstall.dataset.loading = "0";
      settingsCheck.disabled = false;
    }
  }

  // --- Channel picker --------------------------------------------------
  //
  // The segmented control reflects the *active* channel (what
  // /api/updates/check used on the last round-trip). When the user
  // picks a different channel we POST /api/settings/channel with
  // restart:true so the supervisor respawns the worker and the new
  // pick takes effect without a page reload. Beta requires a second
  // confirm because it opts into pre-stable builds.
  let channelActive = "stable";
  let channelBusy = false;
  function setChannelActive(name) {
    channelActive = name || "stable";
    for (const btn of channelButtons) {
      const on = btn.dataset.channel === channelActive;
      btn.setAttribute("aria-checked", on ? "true" : "false");
    }
  }
  async function loadChannel() {
    try {
      const c = await jfetch("/api/settings/channel");
      setChannelActive(c.active || "stable");
      if (c.preference && c.preference !== c.active) {
        channelNote.textContent = `已保存偏好:${c.preference};下次重启生效。`;
      } else if (c.active === "beta") {
        channelNote.textContent = "测试版可能包含未稳定的改动;失败时会自动回滚。";
      } else {
        channelNote.textContent = "稳定版,推荐保持。";
      }
    } catch (err) {
      channelNote.textContent = `加载失败:${err.message || err}`;
    }
  }
  async function setChannel(target) {
    if (channelBusy || target === channelActive) return;
    if (target === "beta") {
      const ok = await promptChannelBeta();
      if (!ok) return;
    }
    channelBusy = true;
    for (const btn of channelButtons) btn.disabled = true;
    channelNote.textContent = `切换到 ${target}…`;
    try {
      const res = await fetch("/api/settings/channel", withAuth({
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ channel: target, restart: true }),
      }));
      const payload = await res.json();
      if (!res.ok) {
        channelNote.textContent = `切换失败:${payload.error || res.status}`;
        return;
      }
      if (payload.restart_requested) {
        channelNote.textContent =
          `已保存 ${payload.preference};worker 正在重启以切换到 ${payload.preference}。`;
        setStatus("error", "即将重启…");
      } else {
        channelNote.textContent = `已保存 ${payload.preference}。`;
      }
    } catch (err) {
      channelNote.textContent = `请求失败:${err.message || err}`;
    } finally {
      channelBusy = false;
      for (const btn of channelButtons) btn.disabled = false;
    }
  }
  function promptChannelBeta() {
    return new Promise((resolve) => {
      const onSubmit = (ev) => {
        ev.preventDefault();
        cleanup();
        if (typeof channelDialog.close === "function") channelDialog.close();
        else channelDialog.removeAttribute("open");
        resolve(true);
      };
      const onCancel = () => {
        cleanup();
        if (typeof channelDialog.close === "function") channelDialog.close();
        else channelDialog.removeAttribute("open");
        resolve(false);
      };
      const cleanup = () => {
        channelForm.removeEventListener("submit", onSubmit);
        channelCancel.removeEventListener("click", onCancel);
      };
      channelForm.addEventListener("submit", onSubmit);
      channelCancel.addEventListener("click", onCancel);
      if (typeof channelDialog.showModal === "function") channelDialog.showModal();
      else channelDialog.setAttribute("open", "");
    });
  }
  for (const btn of channelButtons) {
    btn.addEventListener("click", () => setChannel(btn.dataset.channel));
  }

  // --- Hash-based SPA router -------------------------------------------
  //
  // The legacy Next.js app routed /dashboard, /terminal, /logs, /settings
  // as full pages. v0.1.0 only has a backend for /dashboard + /settings,
  // so Terminal and Logs render a "coming later" stub (see .stub in
  // styles.css) to keep the sidebar tree navigable without shipping
  // half-built screens.
  const VIEW_IDS = ["dashboard", "terminal", "logs", "settings"];
  function currentView() {
    const h = (window.location.hash || "").replace(/^#\/?/, "").split("/")[0];
    return VIEW_IDS.includes(h) ? h : "dashboard";
  }
  function showView(id) {
    for (const v of VIEW_IDS) {
      const panel = document.getElementById(`view-${v}`);
      if (!panel) continue;
      const on = v === id;
      panel.hidden = !on;
      panel.classList.toggle("active", on);
    }
    for (const link of document.querySelectorAll("[data-view]")) {
      link.classList.toggle("active", link.dataset.view === id);
      if (link.tagName === "A") {
        link.setAttribute("aria-current", link.dataset.view === id ? "page" : "false");
      }
    }
  }
  window.addEventListener("hashchange", () => showView(currentView()));
  showView(currentView());

  // Initial paint + background refresh loops. /api/updates/check is
  // kicked off once at boot so the nav-dot can light up without user
  // intervention; subsequent checks happen on demand from the Settings
  // button (24h auto-recheck lands with v0.2 supervisor persistence).
  loadVersion();
  loadChannel();
  poll();
  pollOpenClaw();
  runSettingsCheck().catch(() => {});
  // 3s matches the server-side ActivePollInterval so a fresh snapshot
  // is usually waiting when the client polls; the backend backs off to
  // 5s while the gateway is down, so the extra client cadence mostly
  // just shortens the UX lag after a user action settles.
  setInterval(poll, 5000);
  setInterval(pollOpenClaw, 3000);
})();


