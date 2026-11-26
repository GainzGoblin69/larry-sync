package headless

// filesPage is the File Access page: the card's folders, a text editor, and
// delete. Names on the card are only ever put on the page as text.
const filesPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
<title>Larry Boy Advanced files</title>
<style>
  :root {
    --bg: #0b0b0d; --panel: #17171b; --line: #26262c; --text: #f2f2f2;
    --dim: #9a9aa3; --accent: #ff2d6f; --danger: #ff4d4d; --ok: #3ecf8e;
  }
  * { box-sizing: border-box; }
  html, body { margin: 0; background: var(--bg); color: var(--text);
    font: 16px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  header { position: sticky; top: 0; z-index: 2; background: var(--bg);
    padding: calc(env(safe-area-inset-top) + 14px) 16px 10px; border-bottom: 1px solid var(--line); }
  h1 { margin: 0 0 6px; font-size: 13px; font-weight: 800; letter-spacing: .18em; text-transform: uppercase; color: var(--dim); }
  h1 b { color: var(--text); }
  .crumbs { display: flex; flex-wrap: wrap; gap: 2px; align-items: center; font-size: 15px; }
  .crumbs button { background: none; border: 0; color: var(--accent); font: inherit; padding: 4px 4px; cursor: pointer; }
  .crumbs button:last-child { color: var(--text); font-weight: 700; }
  .crumbs span { color: var(--dim); }
  main { padding: 6px 0 calc(env(safe-area-inset-bottom) + 24px); }
  .row { display: flex; align-items: center; gap: 12px; padding: 12px 16px; border-bottom: 1px solid var(--line); }
  .row .open { flex: 1; min-width: 0; display: flex; gap: 12px; align-items: center; background: none; border: 0;
    color: inherit; font: inherit; text-align: left; padding: 0; cursor: pointer; }
  .icon { width: 28px; text-align: center; font-size: 20px; flex: none; }
  .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .meta { color: var(--dim); font-size: 13px; }
  .hidden .name { color: var(--dim); }
  .del { flex: none; background: none; border: 1px solid var(--line); color: var(--danger); border-radius: 10px;
    padding: 8px 10px; font: inherit; font-size: 14px; cursor: pointer; }
  .lock { flex: none; color: var(--dim); font-size: 13px; }
  .empty, .note { color: var(--dim); padding: 24px 16px; }
  .note { padding: 10px 16px; font-size: 14px; border-bottom: 1px solid var(--line); }
  #editor { position: fixed; inset: 0; z-index: 5; background: var(--bg); display: none; flex-direction: column; }
  #editor.on { display: flex; }
  #editor .bar { display: flex; align-items: center; gap: 8px; padding: calc(env(safe-area-inset-top) + 10px) 12px 10px;
    border-bottom: 1px solid var(--line); }
  #editor .title { flex: 1; min-width: 0; font-weight: 700; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  #editor textarea { flex: 1; width: 100%; border: 0; outline: 0; resize: none; padding: 14px 16px calc(env(safe-area-inset-bottom) + 14px);
    background: var(--panel); color: var(--text); font: 15px/1.5 ui-monospace, Menlo, Consolas, monospace; }
  .btn { border: 0; border-radius: 10px; padding: 9px 14px; font: inherit; font-weight: 700; cursor: pointer; }
  .btn.save { background: var(--accent); color: #fff; }
  .btn.save:disabled { opacity: .45; }
  .btn.close { background: var(--panel); color: var(--text); }
  #toast { position: fixed; left: 50%; bottom: calc(env(safe-area-inset-bottom) + 20px); transform: translateX(-50%);
    background: var(--panel); border: 1px solid var(--line); border-radius: 12px; padding: 10px 16px; z-index: 9;
    max-width: calc(100% - 32px); opacity: 0; transition: opacity .2s; pointer-events: none; }
  #toast.on { opacity: 1; }
  #toast.bad { border-color: var(--danger); }
</style>
</head>
<body>
<header>
  <h1><b>Larry Boy</b> Advanced &middot; files</h1>
  <div class="crumbs" id="crumbs"></div>
</header>
<main id="list"></main>

<div id="editor">
  <div class="bar">
    <button class="btn close" id="close">Close</button>
    <div class="title" id="editing"></div>
    <button class="btn save" id="save" disabled>Save</button>
  </div>
  <textarea id="text" spellcheck="false" autocapitalize="off" autocomplete="off" autocorrect="off"></textarea>
</div>
<div id="toast"></div>

<script>
"use strict";
const key = new URLSearchParams(location.search).get("k") || "";
const $ = (id) => document.getElementById(id);
let here = "";
let editingPath = null;
let saved = "";

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function toast(message, bad) {
  const t = $("toast");
  t.textContent = message;
  t.className = "on" + (bad ? " bad" : "");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.className = ""; }, 3200);
}

async function call(method, endpoint, path, body) {
  const res = await fetch(endpoint + "?path=" + encodeURIComponent(path), {
    method, body, headers: { "X-Key": key }, cache: "no-store",
  });
  if (!res.ok) {
    let message = "Something went wrong (" + res.status + ")";
    try { message = (await res.json()).error || message; } catch (e) {}
    throw new Error(message);
  }
  return res;
}

function size(bytes) {
  if (bytes < 1024) return bytes + " B";
  const units = ["KB", "MB", "GB"];
  let v = bytes / 1024, i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (v < 10 ? v.toFixed(1) : Math.round(v)) + " " + units[i];
}

function when(seconds) {
  return new Date(seconds * 1000).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function join(dir, name) { return dir ? dir + "/" + name : name; }

function crumbs(path) {
  const c = $("crumbs");
  c.replaceChildren();
  const parts = path ? path.split("/") : [];
  const add = (label, target) => {
    const b = el("button", "", label);
    b.onclick = () => go(target);
    c.append(b);
  };
  add("SD card", "");
  parts.forEach((part, i) => {
    c.append(el("span", "", "/"));
    add(part, parts.slice(0, i + 1).join("/"));
  });
}

async function go(path, push = true) {
  let data;
  try {
    data = await (await call("GET", "/api/list", path)).json();
  } catch (e) {
    toast(e.message, true);
    return;
  }
  here = data.path;
  if (push) history.pushState({ path: here }, "", location.pathname + location.search + "#" + encodeURIComponent(here));
  crumbs(here);
  const list = $("list");
  list.replaceChildren();
  if (data.locked) list.append(el("div", "note", "This is the firmware. You can look and download, but not change or delete."));
  if (!data.entries.length) list.append(el("div", "empty", "Nothing in this folder."));
  for (const entry of data.entries) {
    const row = el("div", "row" + (entry.name.startsWith(".") ? " hidden" : ""));
    const open = el("button", "open");
    open.append(el("span", "icon", entry.dir ? "\u{1F4C1}" : entry.text ? "\u{1F4DD}" : "\u{1F4C4}"));
    const label = el("div");
    label.style.minWidth = "0";
    label.append(el("div", "name", entry.name));
    label.append(el("div", "meta", entry.dir ? "Folder" : size(entry.size) + " · " + when(entry.modified)));
    open.append(label);
    const path = join(here, entry.name);
    open.onclick = () => {
      if (entry.dir) go(path);
      else if (entry.text) edit(path, entry.locked);
      else location.href = "/api/download?path=" + encodeURIComponent(path) + "&k=" + encodeURIComponent(key);
    };
    row.append(open);
    if (entry.locked) {
      row.append(el("span", "lock", "\u{1F512}"));
    } else {
      const del = el("button", "del", "Delete");
      del.onclick = () => remove(path, entry);
      row.append(del);
    }
    list.append(row);
  }
}

async function remove(path, entry) {
  const what = entry.dir ? "the folder “" + entry.name + "” and everything in it" : "“" + entry.name + "”";
  if (!confirm("Delete " + what + " from the Hammer?\n\nThis can't be undone.")) return;
  try {
    await call("POST", "/api/delete", path);
    toast("Deleted " + entry.name);
    go(here, false);
  } catch (e) {
    toast(e.message, true);
  }
}

async function edit(path, locked) {
  let text;
  try {
    text = await (await call("GET", "/api/text", path)).text();
  } catch (e) {
    toast(e.message, true);
    return;
  }
  editingPath = path;
  saved = text;
  $("text").value = text;
  $("text").readOnly = !!locked;
  $("editing").textContent = path.split("/").pop() + (locked ? " (read only)" : "");
  $("save").disabled = true;
  $("save").style.display = locked ? "none" : "";
  $("editor").className = "on";
  history.pushState({ path: here, editing: path }, "", location.href);
}

function closeEditor(fromHistory) {
  if ($("text").value !== saved && !confirm("Close without saving your changes?")) {
    if (fromHistory) history.pushState({ path: here, editing: editingPath }, "", location.href);
    return;
  }
  editingPath = null;
  $("editor").className = "";
  if (!fromHistory) history.back();
}

$("text").addEventListener("input", () => { $("save").disabled = $("text").value === saved; });
$("close").onclick = () => closeEditor(false);
$("save").onclick = async () => {
  const value = $("text").value;
  $("save").disabled = true;
  try {
    await call("PUT", "/api/text", editingPath, value);
    saved = value;
    toast("Saved");
    go(here, false);
  } catch (e) {
    $("save").disabled = false;
    toast(e.message, true);
  }
};
window.addEventListener("popstate", (event) => {
  if (editingPath) { closeEditor(true); return; }
  // a jump to a folder's address (#Roms/GBA) carries no state of its own
  const path = event.state && typeof event.state.path === "string" ? event.state.path : decodeURIComponent(location.hash.slice(1) || "");
  go(path, false);
});
window.addEventListener("beforeunload", (event) => {
  if (editingPath && $("text").value !== saved) { event.preventDefault(); event.returnValue = ""; }
});

const start = decodeURIComponent(location.hash.slice(1) || "");
history.replaceState({ path: start }, "", location.href);
go(start, false);
</script>
</body>
</html>
`
