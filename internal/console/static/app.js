const ui = {
  viewer: document.querySelector("#viewer"),
  logout: document.querySelector("#logout"),
  refresh: document.querySelector("#refresh"),
  rescan: document.querySelector("#rescan"),
  refreshStatus: document.querySelector("#refresh-status"),
  totalApps: document.querySelector("#total-apps"),
  runningApps: document.querySelector("#running-apps"),
  attentionApps: document.querySelector("#attention-apps"),
  hourlyRequests: document.querySelector("#hourly-requests"),
  serviceCount: document.querySelector("#service-count"),
  grid: document.querySelector("#app-grid"),
  toast: document.querySelector("#toast"),
};

const state = {
  csrf: "",
  loading: false,
  toastTimer: null,
};

async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body) {
    headers.set("Content-Type", "application/json");
  }
  if (options.method && options.method !== "GET") {
    headers.set("X-CSRF-Token", state.csrf);
  }
  const response = await fetch(path, { ...options, headers });
  if (response.status === 401) {
    window.location.reload();
    throw new Error("Your session expired. Signing in again.");
  }
  if (response.status === 204) {
    return null;
  }
  const data = await response.json();
  if (!response.ok) {
    throw new Error(data.error || `Request failed with status ${response.status}`);
  }
  return data;
}

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function formatNumber(value) {
  return new Intl.NumberFormat().format(Number(value || 0));
}

function formatBytes(bytes) {
  const value = Number(bytes || 0);
  if (value < 1024) return `${value} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let amount = value / 1024;
  let unit = units[0];
  for (let index = 1; amount >= 1024 && index < units.length; index += 1) {
    amount /= 1024;
    unit = units[index];
  }
  return `${amount >= 10 ? amount.toFixed(0) : amount.toFixed(1)} ${unit}`;
}

function formatActivity(value) {
  if (!value || value.startsWith("0001-")) return "No traffic";
  const time = new Date(value);
  const seconds = Math.round((Date.now() - time.getTime()) / 1000);
  if (seconds < 10) return "Just now";
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

function metric(label, value) {
  const item = element("div", "metric");
  item.append(element("span", "", label), element("strong", "", value));
  return item;
}

function processRow(process) {
  const row = element("div", "process-row");
  row.append(
    element("strong", "", process.name),
    element("span", "", `:${process.port || "-"}`),
    element("span", "", `PID ${process.pid || "-"}`),
    element("span", "", `${process.restarts || 0} restarts`),
  );
  return row;
}

function actionButton(label, action, app, disabled, danger = false) {
  const button = element("button", `action-button${danger ? " danger" : ""}`, label);
  button.type = "button";
  button.disabled = disabled;
  button.addEventListener("click", () => runAction(action, app));
  return button;
}

function appCard(app) {
  const card = element("article", "app-card");
  card.dataset.state = app.state;

  const head = element("div", "app-head");
  const identity = element("div");
  identity.append(
    element("h3", "app-name", app.name),
    element("p", "hosts", (app.hosts || []).join("  /  ") || "No host configured"),
  );
  head.append(identity, element("span", "state-badge", app.state));
  card.append(head);

  if (app.error) {
    card.append(element("p", "error-line", app.error));
  }

  const metrics = element("div", "metrics");
  metrics.append(
    metric("Uptime", app.uptime || "-"),
    metric("Memory", formatBytes(app.resources?.memory_bytes)),
    metric("Last activity", formatActivity(app.last_activity)),
    metric("Requests / hour", formatNumber(app.request_rates?.last_hour)),
  );
  card.append(metrics);

  const processes = element("div", "process-list");
  const list = app.processes || [];
  if (list.length) {
    list.forEach((process) => processes.append(processRow(process)));
  } else {
    processes.append(element("div", "process-row", "No active processes"));
  }
  card.append(processes);

  const actions = element("div", "app-actions");
  const changing = app.state === "starting" || app.state === "stopping";
  actions.append(
    actionButton("Start", "start", app.name, app.state === "running" || app.state === "starting"),
    actionButton("Restart", "restart", app.name, changing || app.state === "stopped"),
    actionButton("Stop", "stop", app.name, changing || app.state === "stopped", true),
  );
  card.append(actions);

  return card;
}

function render(apps, updatedAt = new Date().toISOString()) {
  const fleet = Array.isArray(apps) ? apps : [];
  const running = fleet.filter((app) => app.state === "running").length;
  const attention = fleet.filter((app) => ["crashed", "starting", "stopping"].includes(app.state)).length;
  const hourly = fleet.reduce((total, app) => total + Number(app.request_rates?.last_hour || 0), 0);

  ui.totalApps.textContent = formatNumber(fleet.length);
  ui.runningApps.textContent = formatNumber(running);
  ui.attentionApps.textContent = formatNumber(attention);
  ui.hourlyRequests.textContent = formatNumber(hourly);
  ui.serviceCount.textContent = `${fleet.length} ${fleet.length === 1 ? "service" : "services"}`;
  ui.refreshStatus.textContent = `Updated ${new Date(updatedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" })}`;
  ui.grid.setAttribute("aria-busy", "false");

  if (!fleet.length) {
    const empty = element("div", "empty-state");
    empty.append(element("strong", "", "No services found"), element("span", "", "Add an app folder to the global config, then rescan."));
    ui.grid.replaceChildren(empty);
    return;
  }
  ui.grid.replaceChildren(...fleet.map(appCard));
}

function toast(message, error = false) {
  window.clearTimeout(state.toastTimer);
  ui.toast.textContent = message;
  ui.toast.classList.toggle("error", error);
  ui.toast.hidden = false;
  state.toastTimer = window.setTimeout(() => {
    ui.toast.hidden = true;
  }, 5000);
}

function setBusy(busy) {
  state.loading = busy;
  ui.refresh.disabled = busy;
  ui.rescan.disabled = busy;
}

async function refresh({ quiet = false } = {}) {
  if (state.loading || document.hidden) return;
  try {
    if (!quiet) setBusy(true);
    const data = await request("/api/apps");
    render(data.apps, data.updated_at);
  } catch (error) {
    ui.refreshStatus.textContent = "Daemon unavailable";
    if (!quiet) toast(error.message, true);
  } finally {
    if (!quiet) setBusy(false);
  }
}

async function runAction(action, app) {
  if (state.loading) return;
  if (["stop", "restart"].includes(action) && !window.confirm(`${action[0].toUpperCase()}${action.slice(1)} ${app}?`)) return;
  try {
    setBusy(true);
    const data = await request("/api/action", { method: "POST", body: JSON.stringify({ app, action }) });
    render(data.apps, data.updated_at);
    toast(`${app}: ${action} requested`);
  } catch (error) {
    toast(error.message, true);
  } finally {
    setBusy(false);
  }
}

async function rescan() {
  if (state.loading) return;
  try {
    setBusy(true);
    const data = await request("/api/rescan", { method: "POST", body: "{}" });
    render(data.apps);
    if (data.warnings?.length) {
      toast(`Rescan finished with ${data.warnings.length} warning(s): ${data.warnings[0]}`, true);
    } else {
      toast("App folders rescanned");
    }
  } catch (error) {
    toast(error.message, true);
  } finally {
    setBusy(false);
  }
}

async function logout() {
  if (state.loading) return;
  try {
    setBusy(true);
    await request("/api/logout", { method: "POST", body: "{}" });
    window.location.assign("/");
  } catch (error) {
    toast(error.message, true);
    setBusy(false);
  }
}

async function bootstrap() {
  try {
    const data = await request("/api/bootstrap");
    state.csrf = data.csrf;
    ui.viewer.textContent = data.viewer;
    render(data.apps, data.updated_at);
  } catch (error) {
    toast(error.message, true);
  }
}

ui.refresh.addEventListener("click", () => refresh());
ui.rescan.addEventListener("click", rescan);
ui.logout.addEventListener("click", logout);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh({ quiet: true });
});

bootstrap();
window.setInterval(() => refresh({ quiet: true }), 5000);
