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
  restartBanner: document.querySelector("#restart-banner"),
  restartKeys: document.querySelector("#restart-keys"),
  configCount: document.querySelector("#config-count"),
  configFiles: document.querySelector("#config-files"),
  configPath: document.querySelector("#config-path"),
  configLocal: document.querySelector("#config-local"),
  configEffective: document.querySelector("#config-effective"),
  configReference: document.querySelector("#config-reference"),
  configValidate: document.querySelector("#config-validate"),
  configSave: document.querySelector("#config-save"),
  gutter: document.querySelector("#gutter"),
  configText: document.querySelector("#config-text"),
  configMessage: document.querySelector("#config-message"),
  conflict: document.querySelector("#conflict"),
  conflictUse: document.querySelector("#conflict-use"),
  conflictText: document.querySelector("#conflict-text"),
  drawer: document.querySelector("#drawer"),
  drawerTitle: document.querySelector("#drawer-title"),
  drawerText: document.querySelector("#drawer-text"),
  drawerClose: document.querySelector("#drawer-close"),
};

const state = {
  csrf: "",
  loading: false,
  toastTimer: null,
  config: {
    files: [],
    current: null,
    // Edits are kept per file so switching files never loses a change.
    buffers: {},
    revisions: {},
    conflict: null,
    busy: false,
  },
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

function maintenanceButton(app) {
  const on = Boolean(app.maintenance);
  const button = element("button", `action-button${on ? " active" : ""}`, on ? "End maintenance" : "Maintenance");
  button.type = "button";
  button.title = on ? "Serve the app again" : "Answer every request with the maintenance page";
  button.addEventListener("click", () => runAction(on ? "maintenance-off" : "maintenance-on", app.name));
  return button;
}

function actionButton(label, action, app, disabled, danger = false) {
  const button = element("button", `action-button${danger ? " danger" : ""}`, label);
  button.type = "button";
  button.disabled = disabled;
  button.addEventListener("click", () => runAction(action, app));
  return button;
}

function logsLink(app) {
  const link = element("a", "action-button log-link", "View logs");
  link.href = `/logs?app=${encodeURIComponent(app.name)}`;
  link.target = "_blank";
  link.rel = "noreferrer";
  return link;
}

function serviceLink(app) {
  const process = (app.processes || []).find((candidate) => candidate.name === app.web_process && candidate.port);
  if (!process) return element("span", "hosts", (app.hosts || []).join("  /  ") || "Service port unavailable");
  const url = `http://lvh.me:${process.port}`;
  const link = element("a", "service-link", url);
  link.href = url;
  link.target = "_blank";
  link.rel = "noreferrer";
  return link;
}

function errorBlock(app) {
  const block = element("div", "error-line");
  block.append(element("strong", "", app.error));
  if (app.error_log?.length) {
    block.append(element("span", "error-log-label", "Last 1000 log lines"));
    block.append(element("pre", "error-log", app.error_log.join("\n")));
  }
  return block;
}

function appCard(app) {
  const card = element("article", "app-card");
  card.dataset.state = app.state;
  card.dataset.maintenance = String(Boolean(app.maintenance));

  const head = element("div", "app-head");
  const identity = element("div");
  const hosts = element("p", "hosts");
  hosts.append(serviceLink(app));
  identity.append(
    element("h3", "app-name", app.name),
    hosts,
  );
  const controls = element("div", "app-controls");
  controls.append(element("span", "state-badge", app.state));
  if (app.maintenance) {
    controls.append(element("span", "state-badge maintenance", "maintenance"));
  }
  head.append(identity, controls);
  card.append(head);

  if (app.error) {
    card.append(errorBlock(app));
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
    logsLink(app),
    maintenanceButton(app),
    actionButton("Start", "start", app.name, app.state === "running" || app.state === "starting"),
    actionButton("Restart", "restart", app.name, changing || app.state === "stopped"),
    actionButton("Stop", "stop", app.name, changing || app.state === "stopped", true),
  );
  controls.append(actions);

  return card;
}

function render(apps, updatedAt = new Date().toISOString()) {
  const fleet = Array.isArray(apps)
    ? [...apps].sort((left, right) => left.name.localeCompare(right.name))
    : [];
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

function renderRestartRequired(keys) {
  const list = Array.isArray(keys) ? keys : [];
  ui.restartBanner.hidden = list.length === 0;
  ui.restartKeys.textContent = list.length ? `${list.join(", ")} changed in the host file and only ${list.length === 1 ? "applies" : "apply"} on the next start.` : "";
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
    toast(`${app}: ${action.replace("-", " ")} requested`);
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
    renderRestartRequired(data.restart_required);
    loadConfigFiles();
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
    renderRestartRequired(data.restart_required);
    await loadConfigFiles();
  } catch (error) {
    toast(error.message, true);
  }
}

// --- Configuration editor ---------------------------------------------------

function currentFile() {
  return state.config.files.find((file) => file.id === state.config.current) || null;
}

function isDirty(file) {
  const buffer = state.config.buffers[file.id];
  return buffer !== undefined && buffer !== file.saved;
}

function renderFileList() {
  const files = state.config.files;
  ui.configCount.textContent = `${files.length} ${files.length === 1 ? "file" : "files"}`;
  ui.configFiles.replaceChildren(...files.map((file) => {
    const button = element("button", `config-file${file.id === state.config.current ? " selected" : ""}`);
    button.type = "button";
    button.dataset.id = file.id;
    const title = element("strong", "", file.id === "host" ? "Host" : file.app);
    if (isDirty(file)) title.append(element("span", "dirty-dot", ""));
    button.append(title, element("span", "", file.source));
    button.addEventListener("click", () => selectFile(file.id));
    return button;
  }));
}

function renderGutter(errorLine = 0) {
  const lines = ui.configText.value.split("\n").length;
  const numbers = [];
  for (let line = 1; line <= lines; line += 1) {
    numbers.push(line === errorLine ? `<span class="error">${line}</span>` : String(line));
  }
  ui.gutter.innerHTML = numbers.join("\n");
  ui.gutter.scrollTop = ui.configText.scrollTop;
}

function showMessage(message, error = false, line = 0) {
  ui.configMessage.hidden = !message;
  ui.configMessage.textContent = line ? `Line ${line}: ${message}` : message;
  ui.configMessage.classList.toggle("error", error);
  renderGutter(line);
  if (line) {
    const lineHeight = parseFloat(getComputedStyle(ui.configText).lineHeight) || 20;
    ui.configText.scrollTop = Math.max(0, (line - 4) * lineHeight);
    ui.gutter.scrollTop = ui.configText.scrollTop;
  }
}

function showConflict(file) {
  state.config.conflict = file;
  ui.conflict.hidden = !file;
  ui.conflictText.textContent = file ? file.contents : "";
}

function renderEditor() {
  const file = currentFile();
  const editable = Boolean(file);
  ui.configText.disabled = !editable;
  ui.configValidate.disabled = !editable || state.config.busy;
  ui.configSave.disabled = !editable || state.config.busy;
  ui.configEffective.disabled = !file?.app;
  ui.configLocal.hidden = !file || file.id === "host" || file.has_local;
  ui.configPath.textContent = file ? file.path : "Select a file";
  ui.configText.value = file ? state.config.buffers[file.id] ?? file.saved : "";
  renderGutter();
  renderFileList();
}

async function loadConfigFiles() {
  const data = await request("/api/config");
  const previous = new Map(state.config.files.map((file) => [file.id, file]));
  state.config.files = (data.files || []).map((file) => ({ ...file, saved: previous.get(file.id)?.saved ?? "" }));
  if (!currentFile() && state.config.files.length) {
    await selectFile(state.config.files[0].id);
    return;
  }
  renderEditor();
}

async function selectFile(id) {
  const file = state.config.files.find((candidate) => candidate.id === id);
  if (!file) return;
  if (state.config.buffers[id] === undefined || file.saved === "") {
    try {
      const loaded = await request(`/api/config/file?id=${encodeURIComponent(id)}`);
      Object.assign(file, loaded, { saved: loaded.contents });
      if (state.config.buffers[id] === undefined) state.config.buffers[id] = loaded.contents;
      state.config.revisions[id] = loaded.revision;
    } catch (error) {
      toast(error.message, true);
      return;
    }
  }
  state.config.current = id;
  showMessage("");
  showConflict(null);
  renderEditor();
}

function setConfigBusy(busy) {
  state.config.busy = busy;
  ui.configValidate.disabled = busy || !currentFile();
  ui.configSave.disabled = busy || !currentFile();
  ui.configLocal.disabled = busy;
}

async function validateConfig() {
  const file = currentFile();
  if (!file || state.config.busy) return;
  try {
    setConfigBusy(true);
    const result = await request("/api/config/validate", { method: "POST", body: JSON.stringify({ id: file.id, contents: ui.configText.value }) });
    if (result.ok) {
      showMessage(`${file.source} is valid.`);
    } else {
      showMessage(result.error, true, result.line);
    }
  } catch (error) {
    showMessage(error.message, true);
  } finally {
    setConfigBusy(false);
  }
}

async function saveConfig() {
  const file = currentFile();
  if (!file || state.config.busy) return;
  const contents = ui.configText.value;
  try {
    setConfigBusy(true);
    const response = await fetch("/api/config/file", {
      method: "PUT",
      headers: { "Content-Type": "application/json", "X-CSRF-Token": state.csrf },
      body: JSON.stringify({ id: file.id, contents, revision: state.config.revisions[file.id] }),
    });
    if (response.status === 401) {
      window.location.reload();
      return;
    }
    const data = await response.json();
    if (response.status === 409) {
      showConflict(data.file);
      showMessage("Saved copy is stale: the file changed on disk.", true);
      return;
    }
    if (!response.ok) {
      showMessage(data.error || `Save failed with status ${response.status}`, true, data.line);
      return;
    }
    Object.assign(file, data.file, { saved: data.file.contents });
    state.config.buffers[file.id] = data.file.contents;
    state.config.revisions[file.id] = data.file.revision;
    showConflict(null);
    showMessage(`Saved ${file.source} and rescanned.`);
    renderRestartRequired(data.restart_required);
    if (data.invalid?.length) {
      toast(`Saved. Rescan found ${data.invalid.length} invalid app(s): ${data.invalid[0]}`, true);
    } else {
      toast(`Saved ${file.source}. Apps rescanned.`);
    }
    await loadConfigFiles();
    refresh({ quiet: true });
  } catch (error) {
    showMessage(error.message, true);
  } finally {
    setConfigBusy(false);
  }
}

function useDiskVersion() {
  const file = currentFile();
  const disk = state.config.conflict;
  if (!file || !disk) return;
  Object.assign(file, disk, { saved: disk.contents });
  state.config.buffers[file.id] = disk.contents;
  state.config.revisions[file.id] = disk.revision;
  showConflict(null);
  showMessage("Loaded the disk version. Your previous edit is gone from the editor.");
  renderEditor();
}

async function createOverride() {
  const file = currentFile();
  if (!file?.app || state.config.busy) return;
  try {
    setConfigBusy(true);
    const created = await request("/api/config/local", { method: "POST", body: JSON.stringify({ app: file.app }) });
    delete state.config.buffers[file.id];
    await loadConfigFiles();
    const entry = state.config.files.find((candidate) => candidate.id === created.id);
    if (entry) Object.assign(entry, created, { saved: created.contents });
    state.config.buffers[created.id] = created.contents;
    state.config.revisions[created.id] = created.revision;
    state.config.current = created.id;
    renderEditor();
    toast(`Created ${created.source} for ${file.app}. Edits now survive deploys.`);
  } catch (error) {
    toast(error.message, true);
  } finally {
    setConfigBusy(false);
  }
}

function openDrawer(title, text) {
  ui.drawerTitle.textContent = title;
  ui.drawerText.textContent = text;
  ui.drawer.hidden = false;
  ui.drawerClose.focus();
}

async function fetchText(path) {
  const response = await fetch(path);
  if (response.status === 401) {
    window.location.reload();
    throw new Error("Your session expired. Signing in again.");
  }
  if (!response.ok) {
    const data = await response.json().catch(() => ({}));
    throw new Error(data.error || `Request failed with status ${response.status}`);
  }
  return response.text();
}

async function showEffective() {
  const file = currentFile();
  if (!file?.app) return;
  try {
    openDrawer(`Effective config: ${file.app}`, await fetchText(`/api/config/effective?app=${encodeURIComponent(file.app)}`));
  } catch (error) {
    toast(error.message, true);
  }
}

async function showReference() {
  try {
    openDrawer("Configuration reference", await fetchText("/api/config/reference"));
  } catch (error) {
    toast(error.message, true);
  }
}

ui.configText.addEventListener("input", () => {
  const file = currentFile();
  if (file) state.config.buffers[file.id] = ui.configText.value;
  renderGutter();
  renderFileList();
});
ui.configText.addEventListener("scroll", () => {
  ui.gutter.scrollTop = ui.configText.scrollTop;
});
ui.configText.addEventListener("keydown", (event) => {
  if (event.key === "Tab") {
    event.preventDefault();
    const { selectionStart, selectionEnd, value } = ui.configText;
    ui.configText.value = `${value.slice(0, selectionStart)}  ${value.slice(selectionEnd)}`;
    ui.configText.selectionStart = selectionStart + 2;
    ui.configText.selectionEnd = selectionStart + 2;
    ui.configText.dispatchEvent(new Event("input"));
  }
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
    event.preventDefault();
    saveConfig();
  }
});
ui.configValidate.addEventListener("click", validateConfig);
ui.configSave.addEventListener("click", saveConfig);
ui.configLocal.addEventListener("click", createOverride);
ui.configEffective.addEventListener("click", showEffective);
ui.configReference.addEventListener("click", showReference);
ui.conflictUse.addEventListener("click", useDiskVersion);
ui.drawerClose.addEventListener("click", () => {
  ui.drawer.hidden = true;
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && !ui.drawer.hidden) ui.drawer.hidden = true;
});
window.addEventListener("beforeunload", (event) => {
  if (state.config.files.some(isDirty)) event.preventDefault();
});

ui.refresh.addEventListener("click", () => refresh());
ui.rescan.addEventListener("click", rescan);
ui.logout.addEventListener("click", logout);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) refresh({ quiet: true });
});

bootstrap();
window.setInterval(() => refresh({ quiet: true }), 5000);
