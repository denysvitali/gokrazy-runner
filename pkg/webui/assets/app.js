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
    throw new Error(text.trim() || `HTTP ${resp.status}`);
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
    throw new Error(text.trim() || `HTTP ${resp.status}`);
  }
  return resp.json();
}

async function getText(url) {
  const resp = await fetch(url, { headers: { Accept: "text/plain" } });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(text.trim() || `HTTP ${resp.status}`);
  }
  return resp.text();
}

// Toasts --------------------------------------------------------------------

function toast(message, kind = "info") {
  const host = qs("#toasts");
  if (!host) return;
  const el = document.createElement("div");
  el.className = `toast toast-${kind}`;
  el.textContent = message;
  host.appendChild(el);
  window.setTimeout(() => {
    el.classList.add("toast-out");
    window.setTimeout(() => el.remove(), 300);
  }, 4000);
}

// Formatting ----------------------------------------------------------------

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

function formatDuration(seconds) {
  if (!seconds || seconds <= 0) return "unknown";
  const s = Math.floor(seconds);
  const days = Math.floor(s / 86400);
  const hours = Math.floor((s % 86400) / 3600);
  const mins = Math.floor((s % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${mins}m`;
  if (mins > 0) return `${mins}m`;
  return `${s}s`;
}

function formatPartition(p) {
  if (!p) return "—";
  if (p === "2") return "A (partition 2)";
  if (p === "3") return "B (partition 3)";
  return `partition ${p}`;
}

function percent(used, total) {
  if (!total) return 0;
  return Math.max(0, Math.min(100, Math.round((used / total) * 100)));
}

// Tabs ----------------------------------------------------------------------

const TABS = ["overview", "runner", "network", "system"];
let activeTab = "overview";

function showTab(name) {
  if (!TABS.includes(name)) name = "overview";
  activeTab = name;
  for (const tab of TABS) {
    const panel = qs(`#panel-${tab}`);
    if (panel) panel.hidden = tab !== name;
    const button = document.querySelector(`.tab[data-tab="${tab}"]`);
    if (button) {
      button.classList.toggle("active", tab === name);
      button.setAttribute("aria-selected", tab === name ? "true" : "false");
    }
  }
  if (window.location.hash !== `#${name}`) {
    window.history.replaceState(null, "", `#${name}`);
  }
  // The logs panel is expensive to poll, so only follow while it is visible.
  if (name !== "runner") stopLogFollow();
}

function bindTabs() {
  document.querySelectorAll(".tab").forEach((btn) => {
    btn.addEventListener("click", () => showTab(btn.dataset.tab));
  });
  document.querySelectorAll("[data-goto]").forEach((btn) => {
    btn.addEventListener("click", () => showTab(btn.dataset.goto));
  });
  window.addEventListener("hashchange", () => {
    showTab(window.location.hash.replace(/^#/, ""));
  });
  showTab(window.location.hash.replace(/^#/, "") || "overview");
}

// Small DOM helpers ---------------------------------------------------------

function makeButton(label, onClick, className) {
  const btn = document.createElement("button");
  btn.type = "button";
  btn.textContent = label;
  if (className) btn.className = className;
  btn.addEventListener("click", onClick);
  return btn;
}

function tile({ label, value, sub, kind, meterPercent }) {
  const el = document.createElement("div");
  el.className = "tile";
  if (kind) el.classList.add(`tile-${kind}`);

  const l = document.createElement("div");
  l.className = "tile-label";
  l.textContent = label;
  el.appendChild(l);

  const v = document.createElement("div");
  v.className = "tile-value";
  v.textContent = value;
  el.appendChild(v);

  if (typeof meterPercent === "number") {
    const meter = document.createElement("div");
    meter.className = "tile-meter";
    const fill = document.createElement("span");
    fill.style.width = `${meterPercent}%`;
    if (meterPercent >= 90) fill.classList.add("err");
    else if (meterPercent >= 75) fill.classList.add("warn");
    meter.appendChild(fill);
    el.appendChild(meter);
  }

  if (sub) {
    const s = document.createElement("div");
    s.className = "tile-sub";
    s.textContent = sub;
    el.appendChild(s);
  }
  return el;
}

function kvRow(list, key, value) {
  if (value === undefined || value === null || value === "") return;
  const dt = document.createElement("dt");
  dt.textContent = key;
  const dd = document.createElement("dd");
  dd.textContent = value;
  list.appendChild(dt);
  list.appendChild(dd);
}

function setBadge(sel, text, kind) {
  const el = qs(sel);
  if (!el) return;
  el.textContent = text;
  el.classList.remove("badge-ok", "badge-warn", "badge-err");
  if (kind) el.classList.add(`badge-${kind}`);
}

// Config --------------------------------------------------------------------

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

function renderStatusPill(status) {
  const pill = qs("#status-pill");
  if (!pill) return;
  const version = status && status.version ? status.version : "unknown";
  const configured = !!(status && status.has_runner_data);
  const hasToken = !!(status && status.has_token);
  const parts = [`v${version}`];
  parts.push(configured ? "configured" : "not configured");
  if (!configured && hasToken) parts.push("token pending");
  pill.textContent = parts.join(" • ");
  pill.classList.toggle("pill-warn", !!(status && status.password_is_default));
  pill.classList.toggle("pill-ok", configured && !(status && status.password_is_default));

  const banner = qs("#default-password-banner");
  if (banner) banner.hidden = !(status && status.password_is_default);
  const setup = qs("#setup-banner");
  if (setup) setup.hidden = configured || hasToken;

  setBadge("#token-badge", hasToken ? "token stored" : "no token", hasToken ? "ok" : "warn");
}

function fillTailscale(payload) {
  if (!payload) payload = {};
  const el = qs("#tailscale-status");
  if (!el) return;
  if (payload.configured) {
    el.textContent = "Auth key configured. Submitting again will replace it.";
    el.classList.add("ok");
    setBadge("#tailscale-badge", "configured", "ok");
  } else {
    el.textContent = "Auth key not configured.";
    el.classList.remove("ok");
    setBadge("#tailscale-badge", "not configured", null);
  }
}

// System / overview ---------------------------------------------------------

let lastSystem = null;

function renderSystem(sys) {
  lastSystem = sys;
  const tiles = qs("#overview-tiles");
  if (!tiles) return;

  if (!sys) {
    tiles.replaceChildren();
    tiles.appendChild(tile({ label: "System", value: "unavailable", kind: "err" }));
    return;
  }

  const host = qs("#topbar-host");
  if (host) host.textContent = sys.hostname || "";

  const runnerKind =
    sys.runner.status === "running" ? "ok" : sys.runner.status === "stopped" ? "err" : "warn";

  const items = [
    tile({
      label: "Runner",
      value: sys.runner.status,
      sub: sys.runner.detail || "",
      kind: runnerKind,
    }),
    tile({ label: "Uptime", value: formatDuration(sys.uptime_seconds) }),
  ];

  if (sys.load_average && sys.load_average.length === 3) {
    items.push(
      tile({
        label: "Load average",
        value: sys.load_average[0].toFixed(2),
        sub: `${sys.load_average[1].toFixed(2)} / ${sys.load_average[2].toFixed(2)} (5m / 15m)`,
      }),
    );
  }

  if (typeof sys.cpu_temp_c === "number") {
    const temp = sys.cpu_temp_c;
    items.push(
      tile({
        label: "CPU temperature",
        value: `${temp.toFixed(1)} °C`,
        // The Pi 4 starts throttling around 80 °C.
        kind: temp >= 80 ? "err" : temp >= 70 ? "warn" : null,
      }),
    );
  }

  if (sys.memory && sys.memory.total_bytes) {
    const used = sys.memory.total_bytes - sys.memory.available_bytes;
    items.push(
      tile({
        label: "Memory",
        value: `${formatBytes(used)} used`,
        sub: `${formatBytes(sys.memory.available_bytes)} available of ${formatBytes(sys.memory.total_bytes)}`,
        meterPercent: percent(used, sys.memory.total_bytes),
      }),
    );
  }

  for (const disk of sys.disks || []) {
    const used = disk.total_bytes - disk.free_bytes;
    items.push(
      tile({
        label: `Disk ${disk.path}`,
        value: `${formatBytes(disk.free_bytes)} free`,
        sub: `${formatBytes(used)} of ${formatBytes(disk.total_bytes)} used`,
        meterPercent: percent(used, disk.total_bytes),
      }),
    );
  }

  tiles.replaceChildren(...items);

  const updated = qs("#overview-updated");
  if (updated) updated.textContent = `updated ${new Date().toLocaleTimeString()}`;

  // Runner container detail
  setBadge("#runner-badge", sys.runner.status, runnerKind);
  const details = qs("#runner-details");
  if (details) {
    details.replaceChildren();
    kvRow(details, "Container", sys.runner.container);
    kvRow(details, "Image", sys.runner.image);
    kvRow(details, "Started", formatDate(sys.runner.started_at));
    kvRow(details, "Detail", sys.runner.detail);
  }

  renderInterfaces(qs("#overview-ifaces"), sys.interfaces);
  renderInterfaces(qs("#network-ifaces"), sys.interfaces);

  const sysDetails = qs("#system-details");
  if (sysDetails) {
    sysDetails.replaceChildren();
    kvRow(sysDetails, "Hostname", sys.hostname);
    kvRow(sysDetails, "Model", sys.model);
    kvRow(sysDetails, "Kernel", sys.kernel);
    kvRow(sysDetails, "Version", sys.version);
    kvRow(sysDetails, "Uptime", formatDuration(sys.uptime_seconds));
  }
}

function renderInterfaces(list, interfaces) {
  if (!list) return;
  list.replaceChildren();
  if (!interfaces || interfaces.length === 0) {
    const li = document.createElement("li");
    li.textContent = "No interfaces reported.";
    list.appendChild(li);
    return;
  }
  for (const ifc of interfaces) {
    const li = document.createElement("li");

    const name = document.createElement("span");
    name.className = "iface-name";
    name.textContent = ifc.name;
    li.appendChild(name);

    const addrs = document.createElement("span");
    addrs.className = "iface-addrs";
    addrs.textContent = (ifc.addresses || []).join(", ") || "no address";
    li.appendChild(addrs);

    const state = document.createElement("span");
    state.className = `badge ${ifc.up ? "badge-ok" : "badge-warn"}`;
    state.textContent = ifc.up ? "up" : "down";
    li.appendChild(state);

    list.appendChild(li);
  }
}

async function loadSystem() {
  try {
    renderSystem(await getJSON("/api/system"));
  } catch (err) {
    renderSystem(null);
  }
}

// Wi-Fi ---------------------------------------------------------------------

function signalBars(dbm) {
  // Typical dBm range for usable Wi-Fi is -30 (excellent) to -90 (unusable).
  if (dbm >= -55) return "▂▄▆█";
  if (dbm >= -67) return "▂▄▆_";
  if (dbm >= -75) return "▂▄__";
  return "▂___";
}

function wifiListItem({ label, meta, actions }) {
  const li = document.createElement("li");
  const text = document.createElement("span");
  text.className = "wifi-label";
  text.textContent = label;
  li.appendChild(text);
  if (meta) {
    const m = document.createElement("span");
    m.className = "wifi-meta";
    m.textContent = meta;
    li.appendChild(m);
  }
  (actions || []).forEach((btn) => li.appendChild(btn));
  return li;
}

function fillWiFi(payload) {
  const statusEl = qs("#wifi-status");
  const savedList = qs("#wifi-saved-list");
  if (!statusEl || !savedList) return;

  if (!payload) {
    statusEl.textContent = "Wi-Fi is not available on this device.";
    statusEl.classList.remove("ok");
    setBadge("#wifi-badge", "unavailable", null);
    savedList.replaceChildren();
    qs("#wifi-scan").disabled = true;
    return;
  }

  if (payload.connected) {
    const iface = payload.interface ? ` on ${payload.interface}` : "";
    statusEl.textContent = `Connected to ${payload.ssid}${iface} (${payload.signal} dBm).`;
    statusEl.classList.add("ok");
    setBadge("#wifi-badge", payload.ssid, "ok");
  } else {
    statusEl.textContent = "Not connected to a Wi-Fi network.";
    statusEl.classList.remove("ok");
    setBadge("#wifi-badge", "not connected", null);
  }

  const networks = payload.networks || [];
  savedList.replaceChildren();
  if (networks.length === 0) {
    savedList.appendChild(wifiListItem({ label: "No saved networks." }));
    return;
  }
  networks.forEach((net, i) => {
    const meta = [i === 0 ? "active" : "", net.has_password ? "WPA" : "open"]
      .filter(Boolean)
      .join(" • ");
    savedList.appendChild(
      wifiListItem({
        label: net.ssid,
        meta,
        actions: [makeButton("Forget", () => forgetWiFi(net.ssid))],
      }),
    );
  });
}

async function forgetWiFi(ssid) {
  if (!window.confirm(`Forget "${ssid}"?`)) return;
  const status = qs("#status-wifi");
  setStatus(status, "Removing...", "info");
  try {
    await postJSON("/api/wifi/forget", { ssid });
    setStatus(status, `Forgot ${ssid}.`, "ok");
    toast(`Forgot ${ssid}`, "ok");
    loadAll();
  } catch (err) {
    setStatus(status, err.message || "Remove failed", "err");
  }
}

function renderScanResults(networks) {
  const list = qs("#wifi-scan-list");
  if (!list) return;
  list.replaceChildren();
  list.hidden = false;

  if (!networks || networks.length === 0) {
    list.appendChild(wifiListItem({ label: "No networks found." }));
    return;
  }

  networks.forEach((net) => {
    const meta = [
      `${signalBars(net.signal)} ${net.signal} dBm`,
      net.encrypted ? "WPA" : "open",
      net.connected ? "connected" : "",
      net.saved && !net.connected ? "saved" : "",
    ]
      .filter(Boolean)
      .join(" • ");

    list.appendChild(
      wifiListItem({
        label: net.ssid,
        meta,
        actions: [
          makeButton("Select", () => {
            qs("#wifi-ssid").value = net.ssid;
            const psk = qs("#wifi-psk");
            psk.value = "";
            // An open network needs no passphrase; focus the button instead.
            if (net.encrypted) psk.focus();
          }),
        ],
      }),
    );
  });
}

async function scanWiFi() {
  const status = qs("#status-wifi");
  const button = qs("#wifi-scan");
  setStatus(status, "Scanning… this takes a few seconds.", "info");
  button.disabled = true;
  try {
    const resp = await postJSON("/api/wifi/scan", {});
    const found = (resp && resp.networks) || [];
    renderScanResults(found);
    setStatus(status, `Found ${found.length} network(s).`, "ok");
  } catch (err) {
    setStatus(status, err.message || "Scan failed", "err");
  } finally {
    button.disabled = false;
  }
}

// Logs ----------------------------------------------------------------------

let logTimer = null;

function stopLogFollow() {
  if (logTimer) {
    window.clearInterval(logTimer);
    logTimer = null;
  }
  const box = qs("#logs-follow");
  if (box) box.checked = false;
}

async function loadLogs({ quiet = false } = {}) {
  const status = qs("#status-logs");
  const out = qs("#logs-output");
  const source = qs("#logs-source").value;
  const lines = qs("#logs-lines").value;
  if (!quiet) setStatus(status, "Loading…", "info");
  try {
    const text = await getText(`/api/logs?source=${encodeURIComponent(source)}&lines=${encodeURIComponent(lines)}`);
    // Keep the view pinned to the bottom unless the operator scrolled up.
    const atBottom = out.scrollTop + out.clientHeight >= out.scrollHeight - 40;
    out.textContent = text.trim() ? text : "(no output)";
    if (atBottom) out.scrollTop = out.scrollHeight;
    if (!quiet) setStatus(status, "", null);
  } catch (err) {
    setStatus(status, err.message || "Failed to load logs", "err");
    stopLogFollow();
  }
}

function bindLogs() {
  qs("#logs-refresh").addEventListener("click", () => loadLogs());
  qs("#logs-source").addEventListener("change", () => loadLogs());
  qs("#logs-lines").addEventListener("change", () => loadLogs());

  qs("#logs-follow").addEventListener("change", (e) => {
    if (e.target.checked) {
      loadLogs({ quiet: true });
      logTimer = window.setInterval(() => loadLogs({ quiet: true }), 5000);
    } else {
      stopLogFollow();
    }
  });

  qs("#logs-copy").addEventListener("click", async () => {
    const text = qs("#logs-output").textContent || "";
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        toast("Logs copied to clipboard", "ok");
        return;
      }
      throw new Error("clipboard unavailable");
    } catch (err) {
      const range = document.createRange();
      range.selectNodeContents(qs("#logs-output"));
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      toast("Logs selected — press Ctrl/Cmd+C", "info");
    }
  });
}

// Loading -------------------------------------------------------------------

async function loadAll() {
  try {
    const [status, config, keys, tailscale, wifi] = await Promise.all([
      getJSON("/api/status").catch(() => null),
      getJSON("/api/config").catch(() => null),
      getJSON("/api/keys").catch(() => null),
      getJSON("/api/tailscale").catch(() => null),
      getJSON("/api/wifi/status").catch(() => null),
    ]);
    renderStatusPill(status);
    fillConfig(config);
    fillKeys(keys);
    fillTailscale(tailscale);
    fillWiFi(wifi);
  } catch (err) {
    const pill = qs("#status-pill");
    if (pill) pill.textContent = "error loading";
  }
  await loadSystem();
}

function disablePage() {
  document.querySelectorAll("button, input, textarea, select").forEach((el) => {
    el.disabled = true;
  });
}

function bindForms() {
  qs("#refresh-button").addEventListener("click", () => {
    loadAll();
    loadOtaStatus();
  });

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
      setStatus(status, "Saved. runner-init picks this up within 10s.", "ok");
      toast("Configuration saved", "ok");
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
      toast("Registration token saved", "ok");
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
      toast("Authorized keys saved", "ok");
    } catch (err) {
      setStatus(status, err.message || "Save failed", "err");
    }
  });

  qs("#wifi-scan").addEventListener("click", scanWiFi);

  qs("#wifi-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-wifi");
    const ssid = qs("#wifi-ssid");
    const psk = qs("#wifi-psk");
    setStatus(status, "Saving...", "info");
    try {
      await postJSON("/api/wifi/connect", { ssid: ssid.value.trim(), password: psk.value });
      psk.value = "";
      setStatus(status, "Saved. The device will associate shortly.", "ok");
      toast(`Saved Wi-Fi network ${ssid.value.trim()}`, "ok");
      loadAll();
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
      toast("Tailscale connected", "ok");
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
      setStatus(status, "Password changed. Your browser will ask for it again.", "ok");
      toast("Password changed", "ok");
      loadAll();
    } catch (err) {
      setStatus(status, err.message || "Change failed", "err");
    }
  });

  qs("#runner-restart").addEventListener("click", async () => {
    if (!window.confirm("Restart the runner container?\nA job running right now would be interrupted.")) {
      return;
    }
    const status = qs("#status-runner-control");
    setStatus(status, "Restarting…", "info");
    try {
      const resp = await postJSON("/api/runner/restart", {});
      setStatus(status, (resp && resp.detail) || "Restart requested.", "ok");
      toast("Runner container restarting", "ok");
      window.setTimeout(loadSystem, 3000);
    } catch (err) {
      setStatus(status, err.message || "Restart failed", "err");
    }
  });

  qs("#support-button").addEventListener("click", () => collectSupport({ copy: true }));
  qs("#support-download").addEventListener("click", () => collectSupport({ download: true }));

  qs("#reboot-button").addEventListener("click", async () => {
    if (!window.confirm("Reboot the device now?")) return;
    const status = qs("#status-system");
    setStatus(status, "Rebooting...", "info");
    try {
      await postJSON("/api/reboot", {});
      setStatus(status, "Rebooting... the device will be unreachable for a moment.", "ok");
      stopLogFollow();
      stopAutoRefresh();
      disablePage();
    } catch (err) {
      setStatus(status, err.message || "Reboot failed", "err");
    }
  });
}

// OTA / Software updates ----------------------------------------------------

const OTA_RUNNING_STATES = new Set(["checking", "downloading", "installing"]);
let otaPollTimer = null;

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
    setBadge("#ota-badge", "update available", "warn");
  } else if (payload.releases_error) {
    summary.textContent = `Could not fetch releases: ${payload.releases_error}`;
    summary.classList.add("err");
    summary.classList.remove("ok");
    setBadge("#ota-badge", "unreachable", "err");
  } else if (payload.current_version && payload.latest_version) {
    summary.textContent = "Up to date.";
    summary.classList.remove("err");
    summary.classList.add("ok");
    setBadge("#ota-badge", "up to date", "ok");
  } else {
    summary.textContent = "No releases known.";
    summary.classList.remove("ok", "err");
    setBadge("#ota-badge", "—", null);
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
  qs("#ota-url-install").disabled = running;
  qs("#ota-upload-install").disabled = running;
  qs("#ota-token-state").textContent = payload.has_github_token ? "(configured)" : "(not set)";
  if (payload.releases_error) {
    qs("#ota-manual-wrap").open = true;
  }

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

  qs("#ota-url-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const url = qs("#ota-url").value.trim();
    const status = qs("#status-ota-manual");
    if (!url) {
      setStatus(status, "Enter an image URL first.", "err");
      return;
    }
    if (!window.confirm(`Install image from ${url}?\nThe inactive partition will be flashed and the device will reboot.`)) {
      return;
    }
    setStatus(status, "Starting…", "info");
    try {
      await postJSON("/api/ota/install", { url });
      setStatus(status, "Update started.", "ok");
      schedulePoll(500);
    } catch (err) {
      setStatus(status, err.message || "Failed to start update", "err");
    }
  });

  qs("#ota-upload-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-ota-manual");
    const file = qs("#ota-file").files?.[0];
    if (!file) {
      setStatus(status, "Pick a .squashfs.gz file first.", "err");
      return;
    }
    if (!window.confirm(`Install ${file.name} (${formatBytes(file.size)})?\nThe inactive partition will be flashed and the device will reboot.`)) {
      return;
    }
    setStatus(status, `Uploading ${formatBytes(file.size)}…`, "info");
    try {
      const resp = await fetch(`/api/ota/upload?name=${encodeURIComponent(file.name)}`, {
        method: "POST",
        headers: { "Content-Type": "application/gzip" },
        body: file,
      });
      if (!resp.ok) {
        throw new Error((await resp.text()).trim() || `HTTP ${resp.status}`);
      }
      setStatus(status, "Upload complete; installing.", "ok");
      schedulePoll(500);
    } catch (err) {
      setStatus(status, err.message || "Upload failed", "err");
    }
  });

  qs("#ota-token-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const status = qs("#status-ota-token");
    const input = qs("#ota-token");
    const token = input.value.trim();
    setStatus(status, "Saving…", "info");
    try {
      await postJSON("/api/ota/token", { token });
      input.value = "";
      setStatus(status, token ? "Token saved." : "Token removed.", "ok");
      await loadOtaStatus();
    } catch (err) {
      setStatus(status, err.message || "Failed to save token", "err");
    }
  });
}

async function collectSupport({ copy = false, download = false } = {}) {
  const status = qs("#status-system");
  setStatus(status, "Collecting support logs…", "info");
  let text;
  try {
    text = await getText("/api/support");
  } catch (err) {
    setStatus(status, err.message || "Failed to collect support logs", "err");
    return;
  }

  const wrap = qs("#support-output-wrap");
  const pre = qs("#support-output");
  pre.textContent = text;
  wrap.hidden = false;
  wrap.open = true;

  if (download) {
    const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    const stamp = new Date().toISOString().replace(/[:.]/g, "-");
    a.download = `gokrazy-runner-support-${stamp}.txt`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
    setStatus(status, "Support logs downloaded.", "ok");
    return;
  }

  if (copy) {
    try {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(text);
        setStatus(status, "Support logs copied to clipboard.", "ok");
        return;
      }
    } catch (err) {
      // fall through to manual-copy hint
    }
    const range = document.createRange();
    range.selectNodeContents(pre);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    setStatus(
      status,
      "Clipboard unavailable. The text is selected — press Ctrl/Cmd+C to copy.",
      "info",
    );
    return;
  }

  setStatus(status, "Support logs ready.", "ok");
}

// Auto-refresh --------------------------------------------------------------

let autoRefreshTimer = null;

function startAutoRefresh() {
  stopAutoRefresh();
  autoRefreshTimer = window.setInterval(() => {
    // Refreshing a hidden tab wastes the Pi's CPU and the browser throttles
    // us anyway; pick up again on the next visibilitychange.
    if (document.hidden) return;
    loadSystem();
  }, 10000);
}

function stopAutoRefresh() {
  if (autoRefreshTimer) {
    window.clearInterval(autoRefreshTimer);
    autoRefreshTimer = null;
  }
}

document.addEventListener("visibilitychange", () => {
  if (!document.hidden) loadSystem();
});

document.addEventListener("DOMContentLoaded", () => {
  bindTabs();
  bindForms();
  bindOtaForm();
  bindLogs();
  loadAll();
  loadOtaStatus();
  startAutoRefresh();
});
