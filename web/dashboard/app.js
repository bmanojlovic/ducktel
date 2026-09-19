// ducktel dashboard — plain vanilla JS, no build step, no dependencies.
//
// Talks to the REST API in internal/webapi, which wraps the exact same
// internal/telemetry.Core the MCP tools use. Auth reuses the same read
// token as the MCP server: entered once here, kept in sessionStorage (not
// localStorage, so closing the tab clears it), sent as a Bearer header on
// every /api/* call.

const TOKEN_KEY = "ducktel_token";

const state = {
  tenant: null,
  tenants: [],
};

// --- token / auth ---

function getToken() {
  return sessionStorage.getItem(TOKEN_KEY) || "";
}

function setToken(t) {
  sessionStorage.setItem(TOKEN_KEY, t);
}

function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
}

// api issues an authenticated request to /api/*. On 401 it clears the stored
// token and re-shows the gate, since the token is either wrong or was never
// set (the server has no login flow — a 401 here always means "type the
// token again").
async function api(path, opts) {
  const headers = Object.assign({}, (opts && opts.headers) || {});
  const token = getToken();
  if (token) headers["Authorization"] = "Bearer " + token;

  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) {
    clearToken();
    showGate("Rejected — check the token and try again.");
    throw new Error("unauthorized");
  }
  if (!res.ok) {
    const body = await res.text();
    throw new Error(body || (res.status + " " + res.statusText));
  }
  const ct = res.headers.get("Content-Type") || "";
  if (ct.includes("application/json")) return res.json();
  return null;
}

function showGate(errorMsg) {
  document.getElementById("app").classList.add("hidden");
  document.getElementById("gate").classList.remove("hidden");
  document.getElementById("gate-error").textContent = errorMsg || "";
}

function hideGate() {
  document.getElementById("gate").classList.add("hidden");
  document.getElementById("app").classList.remove("hidden");
}

// --- toast ---

let toastTimer = null;
function toast(msg, isErr) {
  const el = document.getElementById("toast");
  el.textContent = msg;
  el.classList.toggle("err", !!isErr);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.textContent = ""; }, 4000);
}

// --- tenant resolution ---
//
// This is realistically a one-tenant system today. The picker only appears
// if there's genuinely more than one — otherwise tenant selection would be a
// pointless extra click on every load.

async function resolveTenant() {
  const resp = await api("/api/tenants");
  state.tenants = resp.rows.map((r) => r.tenant).filter(Boolean);

  const wrap = document.getElementById("tenant-picker-wrap");
  const label = document.getElementById("tenant-label");
  const select = document.getElementById("tenant-picker");

  if (state.tenants.length === 0) {
    state.tenant = null;
    label.textContent = "no tenants seen yet";
    label.classList.remove("hidden");
    wrap.classList.add("hidden");
    return;
  }

  if (state.tenants.length === 1) {
    state.tenant = state.tenants[0];
    label.textContent = "tenant: " + state.tenant;
    label.classList.remove("hidden");
    wrap.classList.add("hidden");
    return;
  }

  // More than one — show the picker, the multi-tenant future.
  label.classList.add("hidden");
  wrap.classList.remove("hidden");
  select.innerHTML = state.tenants.map((t) => `<option value="${escapeHtml(t)}">${escapeHtml(t)}</option>`).join("");
  state.tenant = state.tenants[0];
  select.value = state.tenant;
  select.onchange = () => {
    state.tenant = select.value;
    loadServices();
  };
}

// --- services (for the traces filter dropdown) ---

async function loadServices() {
  const select = document.getElementById("f-service");
  if (!state.tenant) {
    select.innerHTML = `<option value="">any</option>`;
    return;
  }
  const resp = await api("/api/services?tenant=" + encodeURIComponent(state.tenant));
  const services = resp.rows.map((r) => r.service_name).filter(Boolean);
  select.innerHTML = `<option value="">any</option>` + services
    .map((s) => `<option value="${escapeHtml(s)}">${escapeHtml(s)}</option>`)
    .join("");
}

// --- config (flush button visibility) ---

async function loadConfig() {
  const cfg = await api("/api/config");
  document.getElementById("flush-btn").classList.toggle("hidden", !cfg.flush_enabled);
}

// --- tabs ---

function initTabs() {
  document.querySelectorAll(".tab-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      document.querySelectorAll(".tab-btn").forEach((b) => b.classList.remove("active"));
      btn.classList.add("active");
      const target = btn.dataset.tab;
      document.querySelectorAll(".tab").forEach((sec) => {
        sec.classList.toggle("hidden", sec.id !== "tab-" + target);
      });
    });
  });
}

// --- traces view ---

function initAttrFilters() {
  document.getElementById("f-add-filter").addEventListener("click", () => {
    const row = document.createElement("div");
    row.className = "attr-filter-row";
    row.innerHTML = `
      <input type="text" placeholder="key" class="attr-key">
      <input type="text" placeholder="value" class="attr-value">
      <button type="button" class="secondary remove-filter">×</button>`;
    row.querySelector(".remove-filter").addEventListener("click", () => row.remove());
    document.getElementById("f-attr-filters").appendChild(row);
  });
}

function collectAttrFilters() {
  const rows = document.querySelectorAll("#f-attr-filters .attr-filter-row");
  const out = [];
  rows.forEach((row) => {
    const k = row.querySelector(".attr-key").value.trim();
    const v = row.querySelector(".attr-value").value.trim();
    if (k) out.push(k + ":" + v);
  });
  return out;
}

function statusClass(code) {
  if (code === "STATUS_CODE_ERROR") return "status-error";
  if (code === "STATUS_CODE_OK") return "status-ok";
  return "status-unset";
}

function fmtTime(us) {
  if (!us) return "";
  return new Date(us / 1000).toLocaleString();
}

async function runTraceSearch() {
  if (!state.tenant) {
    toast("no tenant selected — send some telemetry first", true);
    return;
  }
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  const service = document.getElementById("f-service").value;
  if (service) params.set("service_name", service);
  params.set("since_minutes", document.getElementById("f-since").value || "60");
  params.set("limit", document.getElementById("f-limit").value || "100");
  collectAttrFilters().forEach((f) => params.append("filter", f));

  let resp;
  try {
    resp = await api("/api/traces?" + params.toString());
  } catch (e) {
    toast(e.message, true);
    return;
  }

  const tbody = document.querySelector("#traces-table tbody");
  tbody.innerHTML = "";
  document.getElementById("waterfall").classList.add("hidden");

  if (resp.rows.length === 0) {
    tbody.innerHTML = `<tr><td colspan="6" class="empty">no spans matched</td></tr>`;
    return;
  }

  resp.rows.forEach((r) => {
    const tr = document.createElement("tr");
    tr.innerHTML = `
      <td>${fmtTime(r.start_time)}</td>
      <td>${escapeHtml(r.service_name || "")}</td>
      <td>${escapeHtml(r.span_name || "")}</td>
      <td>${(r.duration_ms || 0).toFixed(1)} ms</td>
      <td class="${statusClass(r.status_code)}">${escapeHtml((r.status_code || "").replace("STATUS_CODE_", ""))}</td>
      <td class="mono">${escapeHtml((r.trace_id || "").slice(0, 12))}…</td>`;
    tr.addEventListener("click", () => loadWaterfall(r.trace_id));
    tbody.appendChild(tr);
  });
}

async function loadWaterfall(traceId) {
  let resp;
  try {
    resp = await api("/api/traces/" + encodeURIComponent(traceId) + "?tenant=" + encodeURIComponent(state.tenant));
  } catch (e) {
    toast(e.message, true);
    return;
  }
  const spans = resp.rows;
  const wf = document.getElementById("waterfall");
  wf.classList.remove("hidden");
  if (spans.length === 0) {
    wf.innerHTML = `<div class="empty">no spans</div>`;
    return;
  }

  const minStart = Math.min(...spans.map((s) => s.start_time));
  const maxEnd = Math.max(...spans.map((s) => s.end_time));
  const total = Math.max(maxEnd - minStart, 1);

  // Depth by walking the parent chain; capped so a cyclic/missing parent
  // cannot loop forever.
  const bySpanID = new Map(spans.map((s) => [s.span_id, s]));
  function depthOf(span) {
    let d = 0;
    let cur = span;
    while (cur && cur.parent_span_id && bySpanID.has(cur.parent_span_id) && d < 50) {
      cur = bySpanID.get(cur.parent_span_id);
      d++;
    }
    return d;
  }

  const rows = spans.map((s) => {
    const left = ((s.start_time - minStart) / total) * 100;
    const width = Math.max(((s.end_time - s.start_time) / total) * 100, 0.3);
    const depth = depthOf(s);
    return `
      <div class="wf-row">
        <div class="wf-label" style="padding-left:${depth * 14}px" title="${escapeHtml(s.service_name)} — ${escapeHtml(s.span_name)}">
          ${escapeHtml(s.service_name)} — ${escapeHtml(s.span_name)}
        </div>
        <div class="wf-track">
          <div class="wf-bar ${statusClass(s.status_code)}" style="left:${left}%;width:${width}%"></div>
        </div>
        <div class="wf-duration">${(s.duration_ms || 0).toFixed(1)} ms</div>
      </div>`;
  });
  wf.innerHTML = `<h3>Waterfall — ${escapeHtml(traceId)}</h3>` + rows.join("");
}

// --- metrics view ---

async function runMetricQuery() {
  if (!state.tenant) {
    toast("no tenant selected — send some telemetry first", true);
    return;
  }
  const params = new URLSearchParams();
  params.set("tenant", state.tenant);
  params.set("metric_name", document.getElementById("m-name").value.trim());
  params.set("aggregation", document.getElementById("m-agg").value);
  params.set("since_minutes", document.getElementById("m-since").value || "60");
  const groupBy = document.getElementById("m-groupby").value.trim();
  if (groupBy) params.set("group_by", groupBy);

  const out = document.getElementById("metric-result");
  let resp;
  try {
    resp = await api("/api/metrics?" + params.toString());
  } catch (e) {
    toast(e.message, true);
    return;
  }

  if (resp.rows.length === 0) {
    out.innerHTML = `<div class="empty">no data points matched</div>`;
    return;
  }

  // metric_query aggregates the whole window into one value per group (or
  // one value overall) — not a time series — so a bar comparison (or a
  // single stat when there's nothing to compare) is the right shape, not a
  // line chart.
  const groupCol = resp.columns.find((c) => c !== "value");
  if (!groupCol) {
    out.innerHTML = `<div class="stat-big">${fmtValue(resp.rows[0].value)}</div>`;
    return;
  }

  const max = Math.max(...resp.rows.map((r) => Number(r.value) || 0), 1);
  const bars = resp.rows.map((r) => {
    const v = Number(r.value) || 0;
    const pct = (v / max) * 100;
    return `
      <div class="bar-row">
        <div class="bar-label" title="${escapeHtml(String(r[groupCol]))}">${escapeHtml(String(r[groupCol]))}</div>
        <div class="bar-track"><div class="bar-fill" style="width:${pct}%"></div></div>
        <div class="bar-value">${fmtValue(v)}</div>
      </div>`;
  });
  out.innerHTML = `<div class="bar-chart">${bars.join("")}</div>`;
}

function fmtValue(v) {
  if (v === null || v === undefined) return "—";
  const n = Number(v);
  return Number.isInteger(n) ? String(n) : n.toFixed(3);
}

// --- flush ---

async function runFlush() {
  try {
    await api("/api/flush", { method: "POST" });
    toast("flushed — buffered telemetry is now on disk");
  } catch (e) {
    toast(e.message, true);
  }
}

// --- misc ---

function escapeHtml(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

// --- boot ---

async function connect() {
  try {
    await resolveTenant();
  } catch (e) {
    if (e.message !== "unauthorized") toast(e.message, true);
    return;
  }
  hideGate();
  await loadServices();
  await loadConfig();
}

function initGate() {
  const input = document.getElementById("gate-token");
  const stored = getToken();
  if (stored) input.value = "";

  document.getElementById("gate-connect").addEventListener("click", async () => {
    const v = input.value.trim();
    if (v) setToken(v);
    await connect();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key === "Enter") document.getElementById("gate-connect").click();
  });
}

function init() {
  initGate();
  initTabs();
  initAttrFilters();

  document.getElementById("traces-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runTraceSearch();
  });
  document.getElementById("metrics-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runMetricQuery();
  });
  document.getElementById("flush-btn").addEventListener("click", runFlush);

  // If a token is already stored (same tab, page reload), skip the gate.
  if (getToken()) {
    connect();
  } else {
    showGate();
  }
}

init();
