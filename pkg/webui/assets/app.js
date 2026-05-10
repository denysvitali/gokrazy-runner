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

async function loadAll() {
  try {
    const [status, config, keys] = await Promise.all([
      getJSON("/api/status").catch(() => null),
      getJSON("/api/config").catch(() => null),
      getJSON("/api/keys").catch(() => null),
    ]);
    renderStatusPill(status);
    fillConfig(config);
    fillKeys(keys);
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

document.addEventListener("DOMContentLoaded", () => {
  bindForms();
  loadAll();
});
