// Gokrazy Runner web UI. Plain ES2020+, no dependencies, no build step.

function qs(sel) {
  return document.querySelector(sel);
}

function setStatus(el, msg, kind) {
  if (!el) return;
  el.textContent = msg || "";
  el.classList.remove("ok", "err", "info");
  if (kind) el.classList.add(kind);
}

async function postJSON(url, body) {
  const resp = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(text || `HTTP ${resp.status}`);
  }
  const ct = resp.headers.get("content-type") || "";
  if (ct.includes("application/json")) {
    return resp.json();
  }
  return null;
}

async function getJSON(url) {
  const resp = await fetch(url, { headers: { Accept: "application/json" } });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(text || `HTTP ${resp.status}`);
  }
  return resp.json();
}

function extraObjectToText(extra) {
  if (!extra || typeof extra !== "object") return "";
  const keys = Object.keys(extra).sort();
  return keys.map((k) => `${k}=${extra[k]}`).join("\n");
}

function extraTextToObject(text) {
  const out = {};
  const lines = (text || "").split(/\r?\n/);
  for (const raw of lines) {
    const line = raw.trim();
    if (!line) continue;
    const eq = line.indexOf("=");
    if (eq <= 0) continue;
    const k = line.slice(0, eq).trim();
    const v = line.slice(eq + 1);
    if (!k) continue;
    out[k] = v;
  }
  return out;
}

function renderStatusPill(status) {
  const pill = qs("#status-pill");
  if (!pill) return;
  const version = status && status.version ? status.version : "unknown";
  const configured = !!(status && status.has_runner_data);
  const hasToken = !!(status && status.has_token);
  const parts = [`v${version}`];
  parts.push(configured ? "configured" : "not configured");
  if (!configured && hasToken) parts.push("token pending");
  if (status && status.password_is_default) parts.push("default password");
  pill.textContent = parts.join(" • ");
  pill.classList.toggle("pill-warn", !!(status && status.password_is_default));
  pill.classList.toggle("pill-ok", configured && !(status && status.password_is_default));

  const banner = qs("#default-password-banner");
  if (banner) banner.hidden = !(status && status.password_is_default);
}

function fillConfig(cfg) {
  if (!cfg) cfg = {};
  qs("#cfg-url").value = cfg.url || "";
  qs("#cfg-name").value = cfg.name || "";
  qs("#cfg-labels").value = cfg.labels || "";
  qs("#cfg-image").value = cfg.image || "";
  qs("#cfg-extra").value = extraObjectToText(cfg.extra);
}

function fillKeys(payload) {
  if (!payload) payload = {};
  qs("#keys-value").value = payload.keys || "";
}

function fillTailscale(payload) {
  if (!payload) payload = {};
  const el = qs("#tailscale-status");
  if (!el) return;
  if (payload.configured) {
    el.textContent = "Auth key configured. Submitting again will replace it.";
    el.classList.add("ok");
    el.classList.remove("info");
  } else {
    el.textContent = "Auth key not configured.";
    el.classList.remove("ok");
  }
}

async function loadAll() {
  try {
    const [status, config, keys, tailscale] = await Promise.all([
      getJSON("/api/status").catch(() => null),
      getJSON("/api/config").catch(() => null),
      getJSON("/api/keys").catch(() => null),
      getJSON("/api/tailscale").catch(() => null),
    ]);
    renderStatusPill(status);
    fillConfig(config);
    fillKeys(keys);
    fillTailscale(tailscale);
  } catch (err) {
    const pill = qs("#status-pill");
    if (pill) pill.textContent = "error loading";
  }
}

function disablePage() {
  document.querySelectorAll("button, input, textarea").forEach((el) => {
    el.disabled = true;
  });
}

function bindForms() {
  qs("#config-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-config");
    setStatus(status, "Saving...", "info");
    const body = {
      url: qs("#cfg-url").value.trim(),
      name: qs("#cfg-name").value.trim(),
      labels: qs("#cfg-labels").value.trim(),
      image: qs("#cfg-image").value.trim(),
      extra: extraTextToObject(qs("#cfg-extra").value),
    };
    try {
      await postJSON("/api/config", body);
      setStatus(status, "Saved.", "ok");
      loadAll();
    } catch (err) {
      setStatus(status, err.message || "Save failed", "err");
    }
  });

  qs("#token-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-token");
    const input = qs("#tok-value");
    setStatus(status, "Saving...", "info");
    try {
      await postJSON("/api/token", { token: input.value });
      input.value = "";
      setStatus(status, "Token saved.", "ok");
      loadAll();
    } catch (err) {
      setStatus(status, err.message || "Save failed", "err");
    }
  });

  qs("#keys-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-keys");
    setStatus(status, "Saving...", "info");
    try {
      await postJSON("/api/keys", { keys: qs("#keys-value").value });
      setStatus(status, "Saved.", "ok");
    } catch (err) {
      setStatus(status, err.message || "Save failed", "err");
    }
  });

  qs("#tailscale-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-tailscale");
    const input = qs("#ts-authkey");
    setStatus(status, "Connecting...", "info");
    try {
      await postJSON("/api/tailscale", { auth_key: input.value });
      input.value = "";
      setStatus(status, "Connected.", "ok");
      loadAll();
    } catch (err) {
      setStatus(status, err.message || "Connect failed", "err");
    }
  });

  qs("#password-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-password");
    const oldEl = qs("#pw-old");
    const newEl = qs("#pw-new");
    setStatus(status, "Updating...", "info");
    try {
      await postJSON("/api/password", { old: oldEl.value, new: newEl.value });
      oldEl.value = "";
      newEl.value = "";
      setStatus(status, "Password changed.", "ok");
      loadAll();
    } catch (err) {
      setStatus(status, err.message || "Change failed", "err");
    }
  });

  qs("#reboot-button").addEventListener("click", async () => {
    if (!window.confirm("Reboot the device now?")) return;
    const status = qs("#status-system");
    setStatus(status, "Rebooting...", "info");
    try {
      await postJSON("/api/reboot", {});
      setStatus(status, "Rebooting... the device will be unreachable for a moment.", "ok");
      disablePage();
    } catch (err) {
      setStatus(status, err.message || "Reboot failed", "err");
    }
  });
}

// OTA / Software updates ----------------------------------------------------

const OTA_RUNNING_STATES = new Set(["checking", "downloading", "installing"]);
let otaPollTimer = null;

function formatBytes(n) {
  if (!n || n <= 0) return "unknown";
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

function formatRate(bps) {
  if (!bps || bps <= 0) return "";
  return `${formatBytes(Math.round(bps))}/s`;
}

function formatDate(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

function formatPartition(p) {
  if (!p) return "—";
  if (p === "2") return "A (partition 2)";
  if (p === "3") return "B (partition 3)";
  return `partition ${p}`;
}

function describeOtaPhase(status) {
  if (!status) return "";
  const phase = status.phase || status.state;
  switch (phase) {
    case "checking":
      return "Checking GitHub releases…";
    case "downloading":
    case "flashing": {
      const dl = status.downloaded_bytes ? formatBytes(status.downloaded_bytes) : "";
      const total = status.total_bytes ? formatBytes(status.total_bytes) : "";
      const rate = formatRate(status.download_speed_bps);
      const parts = ["Downloading and flashing"];
      if (dl && total) parts.push(`${dl} / ${total}`);
      else if (dl) parts.push(dl);
      if (rate) parts.push(rate);
      return parts.join(" • ");
    }
    case "switching":
      return "Switching root partition…";
    case "rebooting":
      return "Requesting reboot…";
    case "installed":
      return status.message || "Installed; reboot requested.";
    case "failed":
      return status.error || status.message || "Update failed.";
    default:
      return status.message || "";
  }
}

function renderOtaStatus(payload) {
  if (!payload) return;
  qs("#ota-current").textContent = payload.current_version || "unknown";
  qs("#ota-latest").textContent = payload.latest_version || "unknown";
  qs("#ota-active").textContent = formatPartition(payload.ab_partitions?.active);
  qs("#ota-target").textContent = formatPartition(payload.ab_partitions?.update_slot);

  const summary = qs("#ota-summary");
  if (payload.update_available) {
    summary.textContent = `Update available: ${payload.latest_version}`;
    summary.classList.add("ok");
    summary.classList.remove("err");
  } else if (payload.releases_error) {
    summary.textContent = `Could not fetch releases: ${payload.releases_error}`;
    summary.classList.add("err");
    summary.classList.remove("ok");
  } else if (payload.current_version && payload.latest_version) {
    summary.textContent = "Up to date.";
    summary.classList.remove("err");
    summary.classList.add("ok");
  } else {
    summary.textContent = "No releases known.";
    summary.classList.remove("ok", "err");
  }

  const select = qs("#ota-release");
  const releases = Array.isArray(payload.releases) ? payload.releases : [];
  const previousValue = select.value;
  select.innerHTML = "";
  if (releases.length === 0) {
    const opt = document.createElement("option");
    opt.textContent = "No installable releases found";
    opt.value = "";
    select.appendChild(opt);
    select.disabled = true;
  } else {
    select.disabled = false;
    for (const r of releases) {
      const opt = document.createElement("option");
      opt.value = r.tag_name;
      const date = formatDate(r.published_at);
      const size = formatBytes(r.asset_size);
      const installed = r.installed ? " (installed)" : "";
      opt.textContent = `${r.tag_name} — ${date} (${size})${installed}`;
      select.appendChild(opt);
    }
    if (releases.some((r) => r.tag_name === previousValue)) {
      select.value = previousValue;
    }
  }
  updateOtaReleaseInfo(releases);
  select.onchange = () => updateOtaReleaseInfo(releases);

  const running = OTA_RUNNING_STATES.has(payload.state);
  const wrap = qs("#ota-progress-wrap");
  const showProgress = running || payload.state === "installed" || payload.state === "failed";
  wrap.hidden = !showProgress;
  if (showProgress) {
    const fill = qs("#ota-progress-fill");
    const pct = Math.max(0, Math.min(100, payload.progress_percent || 0));
    fill.style.width = `${pct}%`;
    qs("#ota-progress-label").textContent = describeOtaPhase(payload);
  }

  const installBtn = qs("#ota-install");
  installBtn.disabled = running || releases.length === 0;
  qs("#ota-refresh").disabled = running;

  renderOtaHistory(payload.install_history || []);

  if (running) {
    schedulePoll(2000);
  } else {
    clearPoll();
  }
}

function updateOtaReleaseInfo(releases) {
  const tag = qs("#ota-release").value;
  const r = releases.find((x) => x.tag_name === tag);
  const info = qs("#ota-release-info");
  if (!r) {
    info.textContent = "";
    return;
  }
  const parts = [];
  if (r.name && r.name !== r.tag_name) parts.push(r.name);
  if (r.published_at) parts.push(`Published ${formatDate(r.published_at)}`);
  if (r.asset_size) parts.push(`Size ${formatBytes(r.asset_size)}`);
  info.textContent = parts.join(" • ");
}

function renderOtaHistory(history) {
  const wrap = qs("#ota-history-wrap");
  const list = qs("#ota-history");
  list.innerHTML = "";
  if (!history.length) {
    wrap.hidden = true;
    return;
  }
  wrap.hidden = false;
  for (const entry of history.slice(0, 10)) {
    const li = document.createElement("li");
    if (entry.state === "installed") li.classList.add("ok");
    if (entry.state === "failed") li.classList.add("err");
    const when = formatDate(entry.finished_at || entry.started_at);
    const detail = entry.error || entry.message || "";
    li.textContent = `${entry.release || "?"} — ${entry.state}${when ? ` • ${when}` : ""}${detail ? ` — ${detail}` : ""}`;
    list.appendChild(li);
  }
}

async function loadOtaStatus() {
  try {
    const payload = await getJSON("/api/ota/status");
    renderOtaStatus(payload);
  } catch (err) {
    const summary = qs("#ota-summary");
    if (summary) {
      summary.textContent = err.message || "Failed to load update status";
      summary.classList.add("err");
    }
    clearPoll();
  }
}

function schedulePoll(ms) {
  clearPoll();
  otaPollTimer = window.setTimeout(() => {
    otaPollTimer = null;
    loadOtaStatus();
  }, ms);
}

function clearPoll() {
  if (otaPollTimer) {
    window.clearTimeout(otaPollTimer);
    otaPollTimer = null;
  }
}

function bindOtaForm() {
  qs("#ota-refresh").addEventListener("click", async () => {
    setStatus(qs("#status-ota"), "Checking…", "info");
    await loadOtaStatus();
    setStatus(qs("#status-ota"), "", null);
  });

  qs("#ota-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const tag = qs("#ota-release").value;
    if (!tag) return;
    if (!window.confirm(
      `Install ${tag}?\nThe inactive partition will be flashed and the device will reboot.`
    )) {
      return;
    }
    const status = qs("#status-ota");
    setStatus(status, "Starting…", "info");
    try {
      await postJSON("/api/ota/install", { release_tag: tag });
      setStatus(status, "Update started.", "ok");
      schedulePoll(500);
    } catch (err) {
      setStatus(status, err.message || "Failed to start update", "err");
    }
  });
}

document.addEventListener("DOMContentLoaded", () => {
  bindForms();
  bindOtaForm();
  loadAll();
  loadOtaStatus();
});
