// use-browser bridge: serves the use-browser CLI a browser-level CDP endpoint
// backed by chrome.debugger, so it can drive the user's real, running browser
// (logins intact) with no --remote-debugging-port, no profile copy and no
// "Allow remote debugging?" dialog.
//
//   use-browser CLI --(TCP, token)--> native host (use-browser itself,
//   started by the browser) --(native messaging)--> this service worker
//   --> chrome.debugger / chrome.tabs
//
// The CLI speaks plain CDP JSON-RPC. Page-session commands go straight to
// chrome.debugger.sendCommand. Browser-level Target.* commands do not exist
// for an extension, so they are emulated here on top of chrome.tabs. Session
// ids are the target ids themselves.
//
// The user stays in charge: the browser shows its "started debugging" bar
// while a tab is attached, and pressing Cancel there, or clicking the
// toolbar icon, turns the bridge off until the icon is clicked again.

const HOST = "com.usebrowser.bridge";
const IDLE_DETACH_MS = 5 * 60 * 1000;
const GROUP_TITLE = "use-browser";

let port = null;
let paused = false;
let lastError = "";
const attached = new Map(); // targetId -> idle timer
const conns = new Map(); // CLI connection id -> Set of targetIds it attached

// ---- native host connection ----

async function connect() {
  if (port) return;
  const st = await chrome.storage.local.get(["paused", "instance"]);
  paused = !!st.paused;
  let instance = st.instance;
  if (!instance) {
    instance = crypto.randomUUID().slice(0, 8);
    await chrome.storage.local.set({ instance });
  }
  // build.json is written next to these files by use-browser; its hash tells
  // the CLI whether this copy is the one it carries.
  const build = await fetch(chrome.runtime.getURL("build.json"))
    .then((r) => r.json())
    .then((j) => j.build || "")
    .catch(() => "");
  if (port) return; // another event connected while we awaited
  try {
    port = chrome.runtime.connectNative(HOST);
  } catch (e) {
    lastError = String(e.message || e);
    updateBadge();
    return;
  }
  port.onMessage.addListener(onHostMessage);
  port.onDisconnect.addListener(() => {
    lastError = chrome.runtime.lastError ? chrome.runtime.lastError.message : "native host exited";
    port = null;
    conns.clear();
    detachAll();
    updateBadge();
  });
  port.postMessage({
    hello: { browser: browserName(), instance, version: chrome.runtime.getManifest().version, build },
  });
  lastError = "";
  updateBadge();
}

function browserName() {
  const brands = (navigator.userAgentData && navigator.userAgentData.brands || []).map((b) => b.brand);
  if (brands.includes("Microsoft Edge")) return "edge";
  if (brands.includes("Brave") || navigator.brave) return "brave";
  if (brands.includes("Opera")) return "opera";
  if (brands.includes("Vivaldi")) return "vivaldi";
  if (brands.includes("Google Chrome")) return "chrome";
  return "chromium";
}

function send(c, m) {
  if (port) port.postMessage({ c, m });
}

async function onHostMessage(env) {
  if (env.close) {
    conns.delete(env.c);
    return;
  }
  const m = env.m || {};
  // A new use-browser has rewritten this folder: reload from it. Allowed
  // while turned off, since the off switch survives a reload.
  if (m.method === "UseBrowser.reload") {
    send(env.c, { id: m.id, result: {} });
    setTimeout(() => chrome.runtime.reload(), 100);
    return;
  }
  let reply;
  try {
    if (paused) throw new Error("use-browser is turned off in this browser (click the use-browser bridge icon to turn it back on)");
    const result = m.sessionId ? await sessionCall(m) : await browserCall(env.c, m);
    reply = { id: m.id, result: result || {} };
  } catch (e) {
    reply = { id: m.id, error: { code: -32000, message: errorText(e) } };
  }
  if (m.sessionId) reply.sessionId = m.sessionId;
  send(env.c, reply);
}

// chrome.debugger rejects with the CDP error object serialized as JSON.
function errorText(e) {
  const s = String((e && e.message) || e);
  try {
    const o = JSON.parse(s);
    if (o && o.message) return o.message;
  } catch (_) {}
  return s;
}

// ---- browser-level CDP, emulated ----

async function browserCall(c, m) {
  const p = m.params || {};
  switch (m.method) {
    case "Target.getTargets":
      return { targetInfos: await pageTargets() };
    case "Target.attachToTarget": {
      await ensureAttached(p.targetId);
      if (!conns.has(c)) conns.set(c, new Set());
      conns.get(c).add(p.targetId);
      return { sessionId: p.targetId };
    }
    case "Target.createTarget":
      return { targetId: await createTab(p.url || "about:blank") };
    case "Target.activateTarget": {
      const tab = await tabFor(p.targetId);
      await chrome.tabs.update(tab.id, { active: true });
      await chrome.windows.update(tab.windowId, { focused: true });
      return {};
    }
    case "Target.closeTarget": {
      const tab = await tabFor(p.targetId);
      await chrome.tabs.remove(tab.id);
      return { success: true };
    }
    case "Browser.getVersion":
      return { protocolVersion: "1.3", product: browserName(), userAgent: navigator.userAgent, jsVersion: "" };
    case "Browser.close":
      throw new Error("refusing to close the user's own browser; close tabs instead");
    default:
      throw new Error(`${m.method} is not available through the extension bridge (page-level commands are)`);
  }
}

// pageTargets lists real tabs in tab-strip order (window by window), which is
// at least what a human sees, even though the CLI never relies on order.
async function pageTargets() {
  const [targets, tabs] = await Promise.all([chrome.debugger.getTargets(), chrome.tabs.query({})]);
  const pos = new Map(tabs.map((t) => [t.id, [t.windowId, t.index]]));
  return targets
    .filter((t) => t.type === "page" && t.tabId !== undefined && pos.has(t.tabId))
    .sort((a, b) => {
      const [wa, ia] = pos.get(a.tabId);
      const [wb, ib] = pos.get(b.tabId);
      return wa - wb || ia - ib;
    })
    .map((t) => ({ targetId: t.id, type: "page", title: t.title, url: t.url, attached: t.attached }));
}

async function tabFor(targetId) {
  const t = (await chrome.debugger.getTargets()).find((x) => x.id === targetId);
  if (!t || t.tabId === undefined) throw new Error(`no tab with target id ${targetId}`);
  return chrome.tabs.get(t.tabId);
}

// createTab opens agent tabs in the background, grouped under "use-browser",
// so an agent never takes over the tab the user is looking at.
async function createTab(url) {
  const tab = await chrome.tabs.create({ url, active: false });
  try {
    const [group] = await chrome.tabGroups.query({ title: GROUP_TITLE, windowId: tab.windowId });
    if (group) {
      await chrome.tabs.group({ tabIds: [tab.id], groupId: group.id });
    } else {
      const groupId = await chrome.tabs.group({ tabIds: [tab.id], createProperties: { windowId: tab.windowId } });
      await chrome.tabGroups.update(groupId, { title: GROUP_TITLE, color: "blue" });
    }
  } catch (_) {
    // grouping is cosmetic; a browser without tab groups still gets the tab
  }
  // The target id appears a moment after the tab does.
  for (let i = 0; i < 50; i++) {
    const t = (await chrome.debugger.getTargets()).find((x) => x.tabId === tab.id);
    if (t) return t.id;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error("tab created but its target id never appeared");
}

// ---- page sessions ----

async function sessionCall(m) {
  const targetId = m.sessionId;
  // The CLI may outlive an idle detach, so attach again transparently.
  await ensureAttached(targetId);
  return chrome.debugger.sendCommand({ targetId }, m.method, m.params || {});
}

async function ensureAttached(targetId) {
  if (!targetId) throw new Error("targetId is required");
  if (!attached.has(targetId)) {
    try {
      await chrome.debugger.attach({ targetId }, "1.3");
    } catch (e) {
      // Already attached by us (a previous worker life) is fine.
      if (!/already attached/i.test(errorText(e))) throw e;
    }
    attached.set(targetId, null);
    updateBadge();
  }
  touch(targetId);
}

// touch restarts the idle timer: a tab nobody has driven for a while is
// released, which also takes the "started debugging" bar away.
function touch(targetId) {
  clearTimeout(attached.get(targetId));
  attached.set(targetId, setTimeout(() => detach(targetId), IDLE_DETACH_MS));
}

function detach(targetId) {
  clearTimeout(attached.get(targetId));
  attached.delete(targetId);
  chrome.debugger.detach({ targetId }).catch(() => {});
  updateBadge();
}

function detachAll() {
  for (const id of [...attached.keys()]) detach(id);
}

chrome.debugger.onEvent.addListener((source, method, params) => {
  // Child (flat) sessions are not exposed to the CLI.
  if (source.sessionId || !source.targetId) return;
  for (const [c, targets] of conns) {
    if (targets.has(source.targetId)) send(c, { method, params, sessionId: source.targetId });
  }
});

chrome.debugger.onDetach.addListener((source, reason) => {
  const id = source.targetId;
  if (id) {
    clearTimeout(attached.get(id));
    attached.delete(id);
    for (const [c, targets] of conns) {
      if (targets.has(id)) send(c, { method: "Target.detachedFromTarget", params: { sessionId: id, targetId: id } });
    }
  }
  // Cancel on the "started debugging this browser" bar is the user saying
  // stop. Honour it for every tab, not just this one.
  if (reason === "canceled_by_user") setPaused(true);
  updateBadge();
});

// ---- on/off switch and badge ----

async function setPaused(on) {
  paused = on;
  await chrome.storage.local.set({ paused: on });
  if (on) detachAll();
  updateBadge();
}

chrome.action.onClicked.addListener(() => setPaused(!paused));

function updateBadge() {
  let text = "", color = "#5f6368", title;
  if (paused) {
    text = "OFF";
    title = "use-browser bridge is off. Click to let use-browser drive this browser.";
  } else if (!port) {
    text = "!";
    color = "#d93025";
    title = `use-browser bridge cannot reach the use-browser CLI: ${lastError}\nRun: use-browser ext install`;
  } else if (attached.size > 0) {
    text = String(attached.size);
    color = "#1a73e8";
    title = `use-browser is driving ${attached.size} tab(s). Click to turn it off.`;
  } else {
    title = "use-browser bridge is on and idle. Click to turn it off.";
  }
  chrome.action.setBadgeText({ text });
  chrome.action.setBadgeBackgroundColor({ color });
  chrome.action.setTitle({ title });
}

// ---- lifecycle ----

// The open native port keeps this worker alive (Chrome 105+). The alarm is
// the safety net that reconnects if the host dies or the worker was stopped.
chrome.runtime.onStartup.addListener(connect);
chrome.runtime.onInstalled.addListener(connect);
chrome.alarms.create("reconnect", { periodInMinutes: 0.5 });
chrome.alarms.onAlarm.addListener(connect);
connect();
