interface Job {
  id: string; status: string; filename: string; preset: string; progress: number;
  encoder_used: string; error?: string; output_url?: string;
}
interface Preset { id: string; label: string; kind: string; ext: string }
interface Row { tr: HTMLTableRowElement; name: HTMLElement; preset: HTMLElement; status: HTMLElement; bar: HTMLProgressElement; enc: HTMLElement; act: HTMLElement; last: string }

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const rows = new Map<string, Row>();
const streams = new Map<string, EventSource>();
const TERMINAL = ["done", "failed", "canceled"];
const UPLOAD_CONCURRENCY = 2; // the server caps simultaneous uploads; queue the rest here
const MAX_UPLOAD_RETRIES = 4;
const MAX_STREAMS = 4; // client-side cap on EventSources; the rest of the active jobs are polled
const POLL_MS = 3000;
const BACKOFF_BASE_MS = 3000;
const BACKOFF_MAX_MS = 60000;
const OUTPUT_URL = /^\/api\/v1\/jobs\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\/output$/;

interface Item { file: File; preset: string; field: { name: string; value: string }; tries: number }
const pending: Item[] = [];
let uploading = 0;
const failures = new Map<string, number>(); // consecutive stream failures per job
const retryAt = new Map<string, number>(); // earliest time a stream may be reopened
let globalFails = 0; // consecutive stream failures across all jobs (server-wide trouble, e.g. 503 cap)
let globalRetryAt = 0;

// All server-provided strings (filenames, errors, encoder names) go through textContent only.
function el<K extends keyof HTMLElementTagNameMap>(tag: K, text = "", cls = ""): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (text) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

function announce(msg: string) { $("live").textContent = msg; }

// The error area is a list of lines (one per problem) so several failed uploads do not overwrite
// each other. showError("") clears it.
function showError(msg: string) {
  const e = $("error");
  e.replaceChildren();
  if (msg) e.append(el("div", msg));
  e.hidden = !msg;
}

function addError(msg: string) {
  const e = $("error");
  e.append(el("div", msg));
  e.hidden = false;
}

async function problem(res: Response): Promise<string> {
  try {
    const p = await res.json();
    return String(p.detail || p.title || res.statusText || `HTTP ${res.status}`);
  } catch { return res.statusText || `HTTP ${res.status}`; }
}

function problemText(status: number, body: string): string {
  try {
    const p = JSON.parse(body);
    const t = p.detail || p.title;
    if (t) return String(t);
  } catch { /* not JSON */ }
  if (status === 413) return "File is larger than the server's upload limit.";
  return `Upload failed (HTTP ${status})`;
}

function setEmpty() { $("empty").hidden = rows.size > 0 || uploading > 0 || pending.length > 0; }

async function loadPresets() {
  const sel = $<HTMLSelectElement>("preset");
  const n = $("notice");
  try {
    const res = await fetch("/api/v1/presets");
    if (!res.ok) throw new Error(await problem(res));
    const presets: Preset[] = await res.json();
    const prev = sel.value;
    sel.replaceChildren();
    for (const p of presets) {
      const o = el("option", p.label);
      o.value = p.id;
      sel.append(o);
    }
    if (presets.some(p => p.id === prev)) sel.value = prev;
    sel.disabled = presets.length === 0;
    $<HTMLButtonElement>("drop").disabled = presets.length === 0;
    if (presets.length === 0) {
      n.textContent = "No encoder worker is running, so there are no presets and uploads are disabled. Start a worker and this page will update.";
      n.hidden = false;
    } else if (presets.some(p => p.id === "video-av1")) {
      n.textContent = "Not all players can decode AV1. VP9 and MP4 (H.264/H.265) presets remain available if you need wider compatibility.";
      n.hidden = false;
    } else n.hidden = true;
  } catch (e) {
    n.textContent = `Could not load presets: ${e instanceof Error ? e.message : String(e)}`;
    n.hidden = false;
  }
}

function options(preset: string): { name: string; value: string } {
  const num = (id: string) => { const v = $<HTMLInputElement | HTMLSelectElement>(id).value; return v === "" ? undefined : Number(v); };
  const o: Record<string, unknown> = { preset };
  const speed = num("speed"), crf = num("crf"), height = num("height"), abr = num("abr");
  if (speed !== undefined) o.speed = speed;
  if (crf !== undefined) o.crf = crf;
  if (height !== undefined) o.max_height = height;
  if (abr !== undefined) o.audio_bitrate_k = abr;
  if (Object.keys(o).length === 1) return { name: "preset", value: preset };
  return { name: "options", value: JSON.stringify(o) };
}

function makeRow(): Row {
  const tr = el("tr");
  const name = el("td"), preset = el("td"), status = el("td"), enc = el("td"), act = el("td");
  const bar = el("progress");
  bar.max = 100;
  const pt = el("td");
  pt.append(bar);
  tr.append(name, preset, status, pt, enc, act);
  return { tr, name, preset, status, bar, enc, act, last: "" };
}

function render(job: Job) {
  let r = rows.get(job.id);
  if (!r) {
    r = makeRow();
    rows.set(job.id, r);
    $("jobs").prepend(r.tr);
  }
  const terminal = TERMINAL.includes(job.status);
  const label = job.status + (job.error ? ` – ${job.error}` : "");
  r.name.textContent = job.filename;
  r.preset.textContent = job.preset;
  r.status.textContent = label;
  r.status.className = `st-${job.status}`;
  r.bar.value = job.status === "done" ? 100 : job.progress;
  r.bar.setAttribute("aria-label", `Progress of ${job.filename}`);
  r.enc.textContent = job.encoder_used;
  // Rebuild the action cell only when it changes, so keyboard focus is not lost on every tick.
  const key = `${terminal}|${job.output_url && OUTPUT_URL.test(job.output_url) ? job.output_url : ""}`;
  if (key !== r.last) {
    r.last = key;
    const kids: (Node | string)[] = [];
    if (job.output_url && OUTPUT_URL.test(job.output_url)) {
      const a = el("a", "Download", "btn");
      a.href = job.output_url;
      kids.push(a, " ");
    }
    const rm = el("button", terminal ? "Remove" : "Cancel");
    rm.type = "button";
    rm.setAttribute("aria-label", `${terminal ? "Remove" : "Cancel"} ${job.filename}`);
    rm.onclick = () => { void remove(job.id); };
    kids.push(rm);
    r.act.replaceChildren(...kids);
  }
  setEmpty();
  if (terminal) { closeStream(job.id); failures.delete(job.id); retryAt.delete(job.id); } else subscribe(job.id);
}

function closeStream(id: string) {
  streams.get(id)?.close();
  streams.delete(id);
}

function dropRow(id: string) {
  closeStream(id);
  failures.delete(id);
  retryAt.delete(id);
  rows.get(id)?.tr.remove();
  rows.delete(id);
  setEmpty();
}

function subscribe(id: string) {
  if (streams.has(id) || streams.size >= MAX_STREAMS) return; // over the cap: the shared poll covers it
  if (Math.max(globalRetryAt, retryAt.get(id) ?? 0) > Date.now()) return; // backing off: the shared poll covers it
  const es = new EventSource(`/api/v1/jobs/${id}/events`);
  streams.set(id, es);
  es.onmessage = (ev) => {
    let j: Job;
    try { j = JSON.parse(ev.data); } catch { return; }
    failures.delete(id);
    retryAt.delete(id);
    globalFails = 0;
    const was = rows.get(id)?.status.textContent;
    render(j);
    if (TERMINAL.includes(j.status) && was !== rows.get(id)?.status.textContent) {
      announce(`${j.filename}: ${j.status}${j.error ? ` – ${j.error}` : ""}`);
    }
  };
  // The stream ended or failed (server restart, max stream lifetime, 503 busy). Close it so the
  // browser does not auto-reconnect, back off exponentially with jitter, and let the shared poll
  // keep the row fresh until the backoff expires.
  es.onerror = () => {
    closeStream(id);
    const n = (failures.get(id) ?? 0) + 1;
    failures.set(id, n);
    retryAt.set(id, Date.now() + backoffMs(n));
    globalFails++;
    globalRetryAt = Date.now() + backoffMs(globalFails);
  };
}

function backoffMs(n: number): number {
  const base = Math.min(BACKOFF_MAX_MS, BACKOFF_BASE_MS * 2 ** (n - 1));
  return base * (0.5 + Math.random() * 0.5);
}

let polling = false;
// One shared poll serves every active job that has no open stream (cap reached, backing off, or
// the stream failed). It also reopens streams when their backoff has expired and detects jobs
// removed meanwhile (404).
async function poll() {
  if (polling) return;
  const active = [...rows.entries()].filter(([id, r]) => r.last !== "" && !streams.has(id) && !r.last.startsWith("true|"));
  if (active.length === 0) return;
  polling = true;
  try {
    const res = await fetch("/api/v1/jobs");
    if (res.ok) {
      const byId = new Map((await res.json() as Job[]).map(j => [j.id, j]));
      for (const [id] of active) {
        const j = byId.get(id);
        if (j) render(j); // render() reopens a stream when allowed
        else await resync(id); // not in the (bounded) list: ask for it directly
      }
    }
  } catch { /* network down: next tick */ } finally { polling = false; }
}

async function resync(id: string) {
  if (!rows.has(id)) return;
  try {
    const res = await fetch(`/api/v1/jobs/${id}`);
    if (res.status === 404) { dropRow(id); announce("A job was removed."); return; }
    if (res.ok) render(await res.json());
  } catch { /* retry on the next poll */ }
}

async function remove(id: string) {
  try {
    const res = await fetch(`/api/v1/jobs/${id}`, { method: "DELETE" });
    if (res.status === 204 || res.status === 404) dropRow(id);
    else if (res.status === 202) announce("Cancel requested.");
    else showError(await problem(res));
  } catch (e) { showError(`Request failed: ${e instanceof Error ? e.message : String(e)}`); }
}

// ---- upload queue ----
function enqueue(files: File[]) {
  showError("");
  const preset = $<HTMLSelectElement>("preset").value;
  if (!preset) { showError("No preset available; is a worker running?"); return; }
  const field = options(preset);
  const skipped: string[] = [];
  for (const f of files) {
    if (f.size === 0) { skipped.push(`${f.name} (empty or a folder)`); continue; }
    pending.push({ file: f, preset, field, tries: 0 });
  }
  if (skipped.length) addError(`Skipped: ${skipped.join(", ")}`);
  pump();
}

function pump() {
  while (uploading < UPLOAD_CONCURRENCY && pending.length > 0) {
    const item = pending.shift()!;
    uploading++;
    void send(item).finally(() => { uploading--; setEmpty(); pump(); });
  }
  setEmpty();
}

function send(item: Item): Promise<void> {
  return new Promise((resolve) => {
    const { file, preset } = item;
    const r = makeRow();
    r.name.textContent = file.name;
    r.preset.textContent = preset;
    r.status.textContent = "uploading";
    r.bar.max = 100;
    r.bar.value = 0;
    r.bar.setAttribute("aria-label", `Upload progress of ${file.name}`);
    $("jobs").prepend(r.tr);
    $("empty").hidden = true;
    const done = () => { r.tr.remove(); resolve(); };
    const fail = (msg: string) => { addError(`${file.name}: ${msg}`); announce(`${file.name}: upload failed`); done(); };

    const fd = new FormData();
    fd.append(item.field.name, item.field.value); // preset/options MUST precede the file part
    fd.append("file", file); // the filename is display-only on the server

    const xhr = new XMLHttpRequest();
    const cancel = el("button", "Cancel");
    cancel.type = "button";
    cancel.setAttribute("aria-label", `Cancel upload of ${file.name}`);
    cancel.onclick = () => { item.tries = MAX_UPLOAD_RETRIES + 1; xhr.abort(); };
    r.act.append(cancel);
    xhr.upload.onprogress = (e) => { if (e.lengthComputable && e.total > 0) r.bar.value = (e.loaded / e.total) * 100; };
    xhr.onload = () => {
      if (xhr.status === 202) {
        try { render(JSON.parse(xhr.responseText)); announce(`${file.name}: uploaded`); } catch { fail("unexpected server response"); return; }
        r.tr.remove();
        resolve();
      } else if (xhr.status === 503 && item.tries < MAX_UPLOAD_RETRIES) {
        // Server busy (too many uploads / queue full): wait and retry.
        item.tries++;
        r.status.textContent = "server busy, retrying";
        setTimeout(() => { r.tr.remove(); pending.unshift(item); resolve(); }, 3000 * item.tries);
      } else fail(problemText(xhr.status, xhr.responseText));
    };
    xhr.onerror = () => fail("network error");
    xhr.onabort = () => { announce(`${file.name}: upload canceled`); done(); };
    xhr.open("POST", "/api/v1/jobs");
    xhr.send(fd);
  });
}

// Collect dropped files; reject folders (they would upload as empty/invalid files).
function droppedFiles(dt: DataTransfer): File[] {
  const out: File[] = [];
  const folders: string[] = [];
  const items = Array.from(dt.items ?? []);
  if (items.length) {
    for (const it of items) {
      if (it.kind !== "file") continue;
      const entry = (it as DataTransferItem & { webkitGetAsEntry?: () => { isDirectory: boolean; name: string } | null }).webkitGetAsEntry?.();
      if (entry?.isDirectory) { folders.push(entry.name); continue; }
      const f = it.getAsFile();
      if (f) out.push(f);
    }
  } else out.push(...Array.from(dt.files));
  if (folders.length) addError(`Folders are not supported: ${folders.join(", ")}. Drop individual files.`);
  return out;
}

function wire() {
  const drop = $<HTMLButtonElement>("drop"), input = $<HTMLInputElement>("file");
  drop.onclick = () => input.click(); // a real <button>: Enter/Space work natively
  input.onchange = () => { enqueue(Array.from(input.files ?? [])); input.value = ""; };
  for (const n of ["dragenter", "dragover"]) drop.addEventListener(n, (e) => { e.preventDefault(); drop.classList.add("over"); });
  for (const n of ["dragleave", "drop"]) drop.addEventListener(n, (e) => { e.preventDefault(); drop.classList.remove("over"); });
  drop.addEventListener("drop", (e) => {
    const dt = (e as DragEvent).dataTransfer;
    if (!dt) return;
    const files = droppedFiles(dt);
    if (files.length) enqueue(files);
  });
  // Dropping outside the zone must not navigate the tab to the file.
  for (const n of ["dragover", "drop"]) window.addEventListener(n, (e) => e.preventDefault());
}

async function loadJobs() {
  try {
    const res = await fetch("/api/v1/jobs");
    if (!res.ok) throw new Error(await problem(res));
    const list = (await res.json()) as Job[];
    list.reverse().forEach(render);
  } catch (e) { showError(`Could not load jobs: ${e instanceof Error ? e.message : String(e)}`); }
}

async function init() {
  wire();
  await loadPresets();
  setInterval(() => { void loadPresets(); }, 30000);
  await loadJobs();
  setInterval(() => { void poll(); }, POLL_MS);
}
init().catch((e) => showError(String(e)));
