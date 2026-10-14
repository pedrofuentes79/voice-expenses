"use strict";

// Offline-first: the UI reads and writes IndexedDB only. Every server change goes through the
// outbox, which sync() drains in order whenever the server is reachable.

const $ = (id) => document.getElementById(id);

// ---------- IndexedDB ----------

let dbPromise;
function db() {
  dbPromise ??= new Promise((resolve, reject) => {
    const req = indexedDB.open("gastos", 1);
    req.onupgradeneeded = () => {
      req.result.createObjectStore("entries", { keyPath: "id" });
      req.result.createObjectStore("outbox", { keyPath: "seq", autoIncrement: true });
      req.result.createObjectStore("meta");
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
  return dbPromise;
}
const done = (r) => new Promise((res, rej) => { r.onsuccess = () => res(r.result); r.onerror = () => rej(r.error); });
const objectStore = async (name, mode = "readonly") => (await db()).transaction(name, mode).objectStore(name);
const idb = {
  all: async (s) => done((await objectStore(s)).getAll()),
  get: async (s, k) => done((await objectStore(s)).get(k)),
  put: async (s, v, k) => done((await objectStore(s, "readwrite")).put(v, k)),
  del: async (s, k) => done((await objectStore(s, "readwrite")).delete(k)),
  async replaceAll(s, values) {
    const t = (await db()).transaction(s, "readwrite");
    const os = t.objectStore(s);
    os.clear();
    for (const v of values) os.put(v);
    return new Promise((res, rej) => { t.oncomplete = res; t.onerror = () => rej(t.error); });
  },
};

// ---------- State ----------

const state = { entries: [], outbox: [], config: null, online: navigator.onLine, syncing: false,
  month: new Date().toLocaleDateString("sv-SE").slice(0, 7) };

async function reload() {
  [state.entries, state.outbox, state.config] = await Promise.all([
    idb.all("entries"), idb.all("outbox"), idb.get("meta", "config"),
  ]);
  render();
}

async function enqueue(op) {
  // A newer put/delete for the same entry replaces any queued one.
  const target = op.entry?.id ?? op.id;
  for (const o of await idb.all("outbox")) {
    if (o.type !== "recording" && (o.entry?.id ?? o.id) === target) await idb.del("outbox", o.seq);
  }
  await idb.put("outbox", op);
}

async function saveEntry(entry) {
  entry.updated_at = new Date().toISOString();
  await idb.put("entries", entry);
  await enqueue({ type: "put", entry });
  await reload();
  sync();
}

async function deleteEntry(id) {
  await idb.del("entries", id);
  await enqueue({ type: "delete", id });
  await reload();
  sync();
}

// ---------- Sync ----------

async function call(method, url, body, timeoutMs) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const headers = typeof body === "string" ? { "Content-Type": "application/json" } : undefined;
    // A network failure or timeout throws: that means "offline", and sync() stops.
    const res = await fetch(url, { method, body, headers, signal: ctrl.signal, cache: "no-store" });
    const text = await res.text();
    let data = null;
    try { data = JSON.parse(text); } catch {}
    return res.ok ? { ok: true, data } : { ok: false, error: data?.error || `HTTP ${res.status}` };
  } finally {
    clearTimeout(timer);
  }
}

async function getJSON(url) {
  const r = await call("GET", url, null, 15000);
  if (!r.ok) throw new Error(r.error);
  return r.data;
}

function send(op) {
  if (op.type === "recording") {
    const form = new FormData();
    form.append("id", op.id);
    form.append("recorded_at", op.recordedAt);
    form.append("audio", new Blob([op.audio], { type: op.mime }), "audio");
    return call("POST", "api/recordings", form, 120000);
  }
  if (op.type === "put") return call("PUT", `api/entries/${op.entry.id}`, JSON.stringify(op.entry), 15000);
  return call("DELETE", `api/entries/${op.id}`, null, 15000);
}

let syncAgain = false;
async function sync() {
  if (state.syncing) { syncAgain = true; return; }
  if (!navigator.onLine) { state.online = false; render(); return; }
  state.syncing = true;
  render();
  try {
    for (const op of await idb.all("outbox")) {
      const r = await send(op);
      if (r.ok) {
        await idb.del("outbox", op.seq);
        if (op.type === "recording") for (const e of r.data.entries) await idb.put("entries", e);
      } else {
        // Reachable but refused (e.g. transcription failed): keep it and retry on the next sync.
        await idb.put("outbox", { ...op, error: r.error });
      }
      await reload();
    }
    const [entries, config] = await Promise.all([getJSON("api/entries"), getJSON("api/config")]);
    await idb.put("meta", config, "config");
    // Server state, with still-unsent local edits laid on top.
    const byId = new Map(entries.map((e) => [e.id, e]));
    for (const op of await idb.all("outbox")) {
      if (op.type === "put") byId.set(op.entry.id, op.entry);
      if (op.type === "delete") byId.delete(op.id);
    }
    await idb.replaceAll("entries", [...byId.values()]);
    state.online = true;
  } catch {
    state.online = false;
  } finally {
    state.syncing = false;
    await reload();
    if (syncAgain) { syncAgain = false; sync(); }
  }
}

// ---------- Recording ----------

let recorder = null, stream = null, chunks = [], startedAt = 0, recTimer = 0;
const MAX_SECONDS = 120;

const hint = (text) => { $("recHint").textContent = text; };

$("rec").addEventListener("click", () => (recorder ? recorder.stop() : startRecording()));

async function startRecording() {
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: true });
  } catch {
    hint("Sin acceso al micrófono");
    return;
  }
  chunks = [];
  recorder = new MediaRecorder(stream);
  recorder.ondataavailable = (e) => e.data.size && chunks.push(e.data);
  recorder.onstop = finishRecording;
  recorder.start();
  startedAt = Date.now();
  $("rec").classList.add("recording");
  recTimer = setInterval(() => {
    const s = (Date.now() - startedAt) / 1000;
    hint(`Grabando ${fmtDuration(s)} · tocá para terminar`);
    if (s >= MAX_SECONDS) recorder?.stop();
  }, 200);
  hint("Grabando 0:00 · tocá para terminar");
}

async function finishRecording() {
  clearInterval(recTimer);
  stream.getTracks().forEach((t) => t.stop());
  const mime = recorder.mimeType;
  recorder = null;
  $("rec").classList.remove("recording");
  const seconds = (Date.now() - startedAt) / 1000;
  if (seconds < 1) { hint("Muy corto"); setTimeout(() => hint(""), 2000); return; }
  const blob = new Blob(chunks, { type: mime });
  // Stored as an ArrayBuffer: Blobs in IndexedDB have been unreliable on iOS.
  await enqueue({
    type: "recording", id: crypto.randomUUID(), recordedAt: new Date(startedAt).toISOString(),
    mime, seconds, audio: await blob.arrayBuffer(),
  });
  hint(state.online ? "Procesando…" : "Guardado. Se procesa cuando vuelva la conexión.");
  await reload();
  await sync();
  setTimeout(() => hint(""), state.online ? 0 : 3000);
}

// ---------- Rendering ----------

function el(tag, props = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (k === "class") node.className = v;
    else if (k.includes("-")) node.setAttribute(k, v);
    else node[k] = v;
  }
  node.append(...children.filter((c) => c != null && c !== false));
  return node;
}

const fmtDuration = (s) => `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, "0")}`;
const localDate = (d = new Date()) => d.toLocaleDateString("sv-SE");
const fmtNumber = (n) => n.toLocaleString("es-AR", { maximumFractionDigits: 2 });
const fmtAmount = (e) => (e.amount == null ? "—" : `${e.currency === "USD" ? "US$" : "$"} ${fmtNumber(e.amount)}`);

function dayLabel(date) {
  const today = localDate();
  const yesterday = localDate(new Date(Date.now() - 864e5));
  if (date === today) return "Hoy";
  if (date === yesterday) return "Ayer";
  const d = new Date(`${date}T12:00:00`);
  return d.toLocaleDateString("es-AR", { weekday: "short", day: "numeric", month: "short",
    year: d.getFullYear() === new Date().getFullYear() ? undefined : "numeric" });
}

function renderStatus() {
  const unsent = state.outbox.length;
  const parts = [state.syncing ? "Sincronizando…" : state.online ? "Al día" : "Sin conexión"];
  if (unsent && !state.syncing) parts[0] = `${unsent} sin enviar`;
  if (unsent && !state.online && !state.syncing) parts.unshift("Sin conexión");
  $("status").className = state.syncing ? "syncing" : state.online ? "" : "offline";
  $("statusText").textContent = parts.join(" · ");
}

function pendingRow(op) {
  const sub = op.error ? el("span", { class: "err", textContent: op.error.slice(0, 80) })
    : state.online ? "Procesando…" : "Se procesa cuando vuelva la conexión";
  return el("div", { class: "row pending" },
    el("div", { class: "main" },
      el("div", { class: "title", textContent: `Grabación de ${fmtDuration(op.seconds)}` }),
      el("div", { class: "sub" }, sub)),
    el("button", { class: "discard", textContent: "Descartar", onclick: async () => {
      await idb.del("outbox", op.seq);
      await reload();
    } }));
}

function entryRow(e) {
  const review = e.status === "review";
  const title = e.merchant || e.note || e.category || "Sin descripción";
  const category = title === e.category ? null : (e.category ?? "Sin categoría");
  const sub = [category, e.payment_method, e.merchant && e.note].filter(Boolean).join(" · ");
  return el("div", { class: `row${review ? " review" : ""}`, onclick: () => openSheet(e) },
    el("div", { class: "main" },
      el("div", { class: "title" }, title, review && el("span", { class: "chip", textContent: "Revisar" })),
      el("div", { class: "sub", textContent: sub })),
    el("div", { class: "amount", textContent: fmtAmount(e) }),
    review && e.amount != null && el("button", { class: "confirm", textContent: "✓", "aria-label": "Confirmar",
      onclick: (ev) => { ev.stopPropagation(); saveEntry({ ...e, status: "ok" }); } }));
}

const monthLabel = (month) => {
  const d = new Date(`${month}-15T12:00:00`);
  const name = d.toLocaleDateString("es-AR", { month: "long" });
  const label = name[0].toUpperCase() + name.slice(1);
  return d.getFullYear() === new Date().getFullYear() ? label : `${label} ${d.getFullYear()}`;
};

function shiftMonth(month, delta) {
  const [y, m] = month.split("-").map(Number);
  const d = new Date(y, m - 1 + delta, 1);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}`;
}

function goToMonth(month) {
  state.month = month;
  render();
  window.scrollTo({ top: 0 });
}

function render() {
  renderStatus();
  const list = $("list");
  list.replaceChildren();
  const recordings = state.outbox.filter((o) => o.type === "recording");
  if (recordings.length) {
    list.append(el("section", { class: "day" }, el("h2", { textContent: "Sin procesar" }),
      el("div", { class: "list" }, ...recordings.map(pendingRow))));
  }
  const entries = state.entries.filter((e) => e.date.startsWith(state.month)).sort((a, b) =>
    b.date.localeCompare(a.date) || b.created_at.localeCompare(a.created_at));
  let day = null, group = null;
  for (const e of entries) {
    if (e.date !== day) {
      day = e.date;
      group = el("div", { class: "list" });
      list.append(el("section", { class: "day" }, el("h2", { textContent: dayLabel(day) }), group));
    }
    group.append(entryRow(e));
  }

  const totals = {};
  for (const e of entries) if (e.amount != null) totals[e.currency] = (totals[e.currency] ?? 0) + e.amount;
  $("monthName").textContent = monthLabel(state.month);
  $("monthTotal").textContent = Object.keys(totals).length
    ? ["ARS", "USD"].filter((c) => totals[c]).map((c) => fmtAmount({ amount: totals[c], currency: c })).join(" · ")
    : "Sin gastos";
  $("nextMonth").disabled = state.month >= localDate().slice(0, 7);
  $("nextMonth").style.visibility = $("nextMonth").disabled ? "hidden" : "";
  $("empty").hidden = entries.length > 0 || recordings.length > 0;
  $("empty").textContent = `No hay gastos en ${monthLabel(state.month).toLowerCase()}.`;
}

$("prevMonth").addEventListener("click", () => goToMonth(shiftMonth(state.month, -1)));
$("nextMonth").addEventListener("click", () => goToMonth(shiftMonth(state.month, 1)));
$("monthTitle").addEventListener("click", () => goToMonth(localDate().slice(0, 7)));

// Horizontal swipe on the list changes month; vertical scrolling is left alone.
let touchStart = null;
$("list").addEventListener("touchstart", (e) => {
  touchStart = { x: e.touches[0].clientX, y: e.touches[0].clientY };
}, { passive: true });
$("list").addEventListener("touchend", (e) => {
  if (!touchStart) return;
  const dx = e.changedTouches[0].clientX - touchStart.x;
  const dy = e.changedTouches[0].clientY - touchStart.y;
  touchStart = null;
  if (Math.abs(dx) < 70 || Math.abs(dy) > Math.abs(dx) * 0.6) return;
  const next = shiftMonth(state.month, dx < 0 ? 1 : -1);
  if (next <= localDate().slice(0, 7)) goToMonth(next);
}, { passive: true });

// ---------- Edit sheet ----------

let editing = null;

// ---------- Amount calculator ----------
// The expression is kept raw: digits, "," as the decimal separator, and the operators + − × ÷.

const OPS = "+−×÷";
let expr = "";

const currentNumber = () => expr.split(/[+−×÷]/).pop();

function evaluate(text) {
  const tokens = text.match(/\d+(?:,\d*)?|[+−×÷]/g) ?? [];
  if (tokens.length && OPS.includes(tokens.at(-1))) tokens.pop();
  if (!tokens.length) return null;
  // Two passes for precedence: × ÷ first, then + −.
  const terms = [Number(tokens[0].replace(",", "."))];
  const signs = [];
  for (let i = 1; i < tokens.length; i += 2) {
    const op = tokens[i], n = Number(tokens[i + 1].replace(",", "."));
    if (op === "×") terms.push(terms.pop() * n);
    else if (op === "÷") terms.push(n === 0 ? NaN : terms.pop() / n);
    else { signs.push(op); terms.push(n); }
  }
  let total = terms[0];
  signs.forEach((op, i) => { total = op === "+" ? total + terms[i + 1] : total - terms[i + 1]; });
  return Number.isFinite(total) ? Math.round(total * 100) / 100 : null;
}

const toRaw = (n) => String(n).replace(".", ",");

function fmtRawNumber(raw) {
  const [int, dec] = raw.split(",");
  const grouped = int.replace(/^0+(?=\d)/, "").replace(/\B(?=(\d{3})+(?!\d))/g, ".");
  return dec === undefined ? grouped : `${grouped},${dec}`;
}

function renderAmount() {
  const symbol = editing.currency === "USD" ? "US$" : "$";
  const hasOp = /[+−×÷]/.test(expr);
  const value = evaluate(expr);
  $("fExpr").textContent = hasOp ? expr.replace(/\d+(?:,\d*)?/g, fmtRawNumber).replace(/([+−×÷])/g, " $1 ") : "";
  $("fValue").textContent = hasOp
    ? `= ${value == null ? "—" : `${symbol} ${fmtNumber(value)}`}`
    : `${symbol} ${expr ? fmtRawNumber(expr) : "0"}`;
  $("fAmount").classList.toggle("invalid", hasOp && (value == null || value <= 0));
}

function showKeypad(show) {
  $("keypad").hidden = !show;
  $("fAmount").classList.toggle("active", show);
  if (show) document.activeElement?.blur();
}

function pressKey(k) {
  const num = currentNumber();
  if (/^\d$/.test(k)) expr = num === "0" ? expr.slice(0, -1) + k : expr + k;
  else if (k === "000") { if (num && num !== "0") expr += "000"; }
  else if (k === ",") { if (!num.includes(",")) expr += num ? "," : "0,"; }
  else if (OPS.includes(k)) {
    if (!expr) return;
    expr = OPS.includes(expr.at(-1)) ? expr.slice(0, -1) + k : expr + k;
  } else if (k === "⌫") expr = expr.slice(0, -1);
  else if (k === "C") expr = "";
  else if (k === "=" || k === "OK") {
    const value = evaluate(expr);
    if (value != null && value > 0) expr = toRaw(value);
    if (k === "OK" && value != null && value > 0) showKeypad(false);
  }
  renderAmount();
}

$("keypad").addEventListener("click", (ev) => {
  const k = ev.target.closest("button")?.dataset.k;
  if (k) pressKey(k);
});
$("fAmount").addEventListener("click", () => showKeypad($("keypad").hidden));
// Typing in a text field hides the keypad so the two never stack.
$("form").addEventListener("focusin", (ev) => { if (ev.target.matches("input, select")) showKeypad(false); });

function fillSelect(select, values, current) {
  const options = [...new Set([...values, current].filter(Boolean))];
  select.replaceChildren(el("option", { value: "", textContent: "—" }),
    ...options.map((v) => el("option", { value: v, textContent: v })));
  select.value = current ?? "";
}

function setCurrency(currency) {
  for (const b of $("fCurrency").children) b.classList.toggle("on", b.textContent === currency);
}

function openSheet(entry) {
  // A new entry lands in the month on screen: today, or that month's last day.
  const today = state.month === localDate().slice(0, 7) ? localDate()
    : localDate(new Date(Number(state.month.slice(0, 4)), Number(state.month.slice(5)), 0));
  editing = entry ? { ...entry } : {
    id: crypto.randomUUID(), status: "ok", amount: null, currency: "ARS", category: null, merchant: null,
    payment_method: null, note: "", date: today, source: "manual", transcript: null,
    created_at: new Date().toISOString(),
  };
  const cfg = state.config ?? {
    categories: [...new Set(state.entries.map((e) => e.category))],
    payment_methods: [...new Set(state.entries.map((e) => e.payment_method))],
    currencies: ["ARS", "USD"],
  };
  expr = editing.amount == null ? "" : toRaw(editing.amount);
  $("fCurrency").replaceChildren(...cfg.currencies.map((c) =>
    el("button", { type: "button", textContent: c,
      onclick: () => { editing.currency = c; setCurrency(c); renderAmount(); } })));
  setCurrency(editing.currency);
  fillSelect($("fCategory"), cfg.categories, editing.category);
  fillSelect($("fPayment"), cfg.payment_methods, editing.payment_method);
  $("fMerchant").value = editing.merchant ?? "";
  $("fNote").value = editing.note ?? "";
  $("fDate").value = editing.date;
  $("transcript").textContent = editing.transcript ? `“${editing.transcript}”` : "";
  // Streams from the server, so it only plays online.
  $("fAudio").hidden = !editing.recording_id;
  $("fAudio").src = editing.recording_id ? `api/recordings/${editing.recording_id}/audio` : "";
  $("fDelete").hidden = !entry;
  $("fDelete").textContent = "Borrar";
  renderAmount();
  showKeypad(!entry || editing.amount == null);
  $("sheet").showModal();
}

$("form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const amount = evaluate(expr);
  if (amount == null || amount <= 0) { showKeypad(true); $("fAmount").classList.add("invalid"); return; }
  $("sheet").close();
  saveEntry({
    ...editing, amount, status: "ok",
    category: $("fCategory").value || null, payment_method: $("fPayment").value || null,
    merchant: $("fMerchant").value.trim() || null, note: $("fNote").value.trim(),
    date: $("fDate").value || localDate(),
  });
});
$("fCancel").addEventListener("click", () => $("sheet").close());
$("sheet").addEventListener("close", () => $("fAudio").pause());
$("fDelete").addEventListener("click", () => {
  if ($("fDelete").textContent !== "¿Seguro?") { $("fDelete").textContent = "¿Seguro?"; return; }
  $("sheet").close();
  deleteEntry(editing.id);
});
$("add").addEventListener("click", () => openSheet(null));

// ---------- Startup ----------

navigator.serviceWorker?.register("sw.js");
navigator.storage?.persist?.();
window.addEventListener("online", sync);
window.addEventListener("offline", () => { state.online = false; render(); });
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") sync(); });
setInterval(() => { if (document.visibilityState === "visible") sync(); }, 30000);
reload().then(sync);
