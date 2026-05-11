package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type folderNode struct {
	Name string `json:"name"`
}

type selectionState struct {
	ShowAll          bool                `json:"showAll"`
	WholeFolders     map[string]bool     `json:"wholeFolders"`
	SelectedChildren map[string][]string `json:"selectedChildren"`
	UnsupportedRules []string            `json:"unsupportedRules"`
}

type apiState struct {
	Remote         string         `json:"remote"`
	FilterFile     string         `json:"filterFile"`
	RestartCommand string         `json:"restartCommand"`
	Tree           []folderNode   `json:"tree"`
	Selection      selectionState `json:"selection"`
}

type saveRequest struct {
	ShowAll          bool                `json:"showAll"`
	WholeFolders     map[string]bool     `json:"wholeFolders"`
	SelectedChildren map[string][]string `json:"selectedChildren"`
}

type restartResponse struct {
	Message string `json:"message"`
	Output  string `json:"output,omitempty"`
}

type app struct {
	gcloneBin  string
	configFile string
	filterFile string
	mountpoint string
	remote     string
	restartCmd []string
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}

	addr := flag.String("addr", "127.0.0.1:43123", "listen address")
	remote := flag.String("remote", "gc:", "rclone/gclone remote")
	configFile := flag.String("config", filepath.Join(home, ".config", "rclone", "rclone.conf"), "rclone config file")
	filterFile := flag.String("filter-file", filepath.Join(home, ".config", "rclone", "gclone-selective-sync.txt"), "selective sync filter file")
	gcloneBin := flag.String("gclone-bin", "/usr/local/bin/gclone", "gclone binary path")
	mountpoint := flag.String("mountpoint", "", "optional mounted Drive path for folder discovery")
	restartCommand := flag.String("restart-command", "sudo systemctl restart gclone", "command the UI should run to apply filter changes")
	flag.Parse()

	app := &app{
		gcloneBin:  *gcloneBin,
		configFile: *configFile,
		filterFile: *filterFile,
		mountpoint: *mountpoint,
		remote:     *remote,
		restartCmd: strings.Fields(*restartCommand),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/state", app.handleState)
	mux.HandleFunc("/api/children", app.handleChildren)
	mux.HandleFunc("/api/save", app.handleSave)
	mux.HandleFunc("/api/restart", app.handleRestart)

	server := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Printf("Selective sync UI listening on http://%s\n", *addr)
	log.Fatal(server.ListenAndServe())
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (a *app) handleState(w http.ResponseWriter, r *http.Request) {
	tree, err := a.loadTree()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	selection, err := a.loadSelection()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, apiState{
		Remote:         a.remote,
		FilterFile:     a.filterFile,
		RestartCommand: strings.Join(a.restartCmd, " "),
		Tree:           tree,
		Selection:      selection,
	})
}

func (a *app) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req saveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if err := a.saveSelection(req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{
		"message": "Saved filter file. Restart the gclone service to apply changes.",
	})
}

func (a *app) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(a.restartCmd) == 0 {
		http.Error(w, "restart command is not configured", http.StatusInternalServerError)
		return
	}
	log.Printf("restart: running %s", strings.Join(a.restartCmd, " "))
	cmd := exec.Command(a.restartCmd[0], a.restartCmd[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		} else {
			msg += "\n" + err.Error()
		}
		log.Printf("restart: FAILED: %s", msg)
		http.Error(w, msg, http.StatusBadGateway)
		return
	}
	result := strings.TrimSpace(string(out))
	if result != "" {
		log.Printf("restart: %s", result)
	}
	log.Printf("restart: done")
	writeJSON(w, restartResponse{
		Message: "gclone restart completed.",
		Output:  result,
	})
}

func (a *app) loadTree() ([]folderNode, error) {
	topLevel, err := a.listTopDirs()
	if err != nil {
		return nil, err
	}
	tree := make([]folderNode, 0, len(topLevel))
	for _, name := range topLevel {
		tree = append(tree, folderNode{Name: name})
	}
	return tree, nil
}

func (a *app) handleChildren(w http.ResponseWriter, r *http.Request) {
	top := strings.TrimSpace(r.URL.Query().Get("top"))
	if top == "" {
		http.Error(w, "missing top parameter", http.StatusBadRequest)
		return
	}
	children, err := a.listChildDirs(top)
	if err != nil {
		http.Error(w, fmt.Sprintf("list subfolders for %s: %v", top, err), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"top":      top,
		"children": children,
	})
}

func (a *app) listTopDirs() ([]string, error) {
	if a.mountpoint != "" {
		return a.listLocalDirs(a.mountpoint)
	}
	return a.listRemoteDirs(a.remote)
}

func (a *app) listChildDirs(top string) ([]string, error) {
	if a.mountpoint != "" {
		return a.listLocalDirs(filepath.Join(a.mountpoint, top))
	}
	return a.listRemoteDirs(joinRemote(a.remote, top))
}

func (a *app) listRemoteDirs(remote string) ([]string, error) {
	cmd := exec.Command(a.gcloneBin, "lsf", remote, "--dirs-only", "--format", "p", "--config", a.configFile)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s", bytes.TrimSpace(out))
	}
	lines := strings.Split(string(out), "\n")
	var dirs []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		dirs = append(dirs, line)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func (a *app) listLocalDirs(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func (a *app) loadSelection() (selectionState, error) {
	state := selectionState{
		ShowAll:          true,
		WholeFolders:     map[string]bool{},
		SelectedChildren: map[string][]string{},
	}

	data, err := os.ReadFile(a.filterFile)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}

	lines := strings.Split(string(data), "\n")
	nonCommentRules := 0
	children := map[string]map[string]bool{}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		nonCommentRules++
		if line == "- *" || line == "+ /*" {
			continue
		}
		if !strings.HasPrefix(line, "+ /") {
			state.UnsupportedRules = append(state.UnsupportedRules, line)
			continue
		}

		path := strings.TrimPrefix(line, "+ /")
		switch {
		case strings.HasSuffix(path, "/**"):
			path = strings.TrimSuffix(path, "/**")
			parts := strings.Split(path, "/")
			if len(parts) == 1 {
				state.WholeFolders[parts[0]] = true
				continue
			}
			if len(parts) == 2 {
				if children[parts[0]] == nil {
					children[parts[0]] = map[string]bool{}
				}
				children[parts[0]][parts[1]] = true
				continue
			}
		case strings.HasSuffix(path, "/"):
			// Parent folder include created by this tool. Ignore.
			continue
		}

		state.UnsupportedRules = append(state.UnsupportedRules, line)
	}

	if nonCommentRules > 0 {
		state.ShowAll = false
	}

	for top, childSet := range children {
		for child := range childSet {
			state.SelectedChildren[top] = append(state.SelectedChildren[top], child)
		}
		sort.Strings(state.SelectedChildren[top])
	}

	return state, nil
}

func (a *app) saveSelection(req saveRequest) error {
	if err := os.MkdirAll(filepath.Dir(a.filterFile), 0o755); err != nil {
		return err
	}

	if req.ShowAll {
		return os.WriteFile(a.filterFile, []byte(""), 0o644)
	}

	var rules []string
	rules = append(rules,
		"# Managed by selective-sync-gui.go",
		"# Restart the gclone service after editing or saving this file.",
	)

	var topNames []string
	for top := range req.WholeFolders {
		topNames = append(topNames, top)
	}
	for top := range req.SelectedChildren {
		if !contains(topNames, top) {
			topNames = append(topNames, top)
		}
	}
	sort.Strings(topNames)

	for _, top := range topNames {
		if req.WholeFolders[top] {
			rules = append(rules, fmt.Sprintf("+ /%s/**", top))
			continue
		}

		children := append([]string(nil), req.SelectedChildren[top]...)
		sort.Strings(children)
		if len(children) == 0 {
			continue
		}
		rules = append(rules, fmt.Sprintf("+ /%s/", top))
		for _, child := range children {
			rules = append(rules, fmt.Sprintf("+ /%s/%s/**", top, child))
		}
	}

	rules = append(rules, "+ /*", "- *", "")
	return os.WriteFile(a.filterFile, []byte(strings.Join(rules, "\n")), 0o644)
}

func joinRemote(remote, child string) string {
	if strings.HasSuffix(remote, ":") {
		return remote + child
	}
	return strings.TrimRight(remote, "/") + "/" + child
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

var indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Gclone Selective Sync</title>
  <style>
    :root {
      --bg: #f4efe8;
      --panel: #fffaf3;
      --ink: #1e1d1a;
      --muted: #6b655d;
      --line: #d9ccbc;
      --accent: #1f6f5f;
      --accent-soft: #d7ebe6;
      --warn: #8a5a00;
    }
    * { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: Georgia, "Times New Roman", serif;
      background: radial-gradient(circle at top left, #fef8ef 0, #f4efe8 42%, #eee5d9 100%);
      color: var(--ink);
    }
    main { max-width: 820px; margin: 0 auto; padding: 32px 20px 80px; }
    h1 { font-size: clamp(1.8rem, 4vw, 3rem); margin-bottom: 6px; }
    .lede { color: var(--muted); margin-bottom: 6px; }
    .meta { color: var(--muted); font-size: 0.88rem; margin-bottom: 20px; }
    code { font-family: "SFMono-Regular", Consolas, monospace; font-size: 0.9em; }
    .toolbar { display: flex; flex-wrap: wrap; gap: 10px; margin-bottom: 14px; }
    button {
      border: 0; border-radius: 999px; padding: 10px 20px;
      background: var(--accent); color: white; font: inherit; cursor: pointer;
      transition: opacity .15s;
    }
    button.secondary { background: #e9ddcf; color: var(--ink); }
    button:disabled { opacity: 0.45; cursor: wait; }
    .search-box {
      width: 100%; padding: 10px 16px;
      border: 1px solid var(--line); border-radius: 999px;
      background: #fffdf8; font: inherit; font-size: 1rem;
      margin-bottom: 14px; display: block;
    }
    .warning {
      background: #fff1cf; color: var(--warn);
      border-radius: 10px; padding: 10px 14px; margin-bottom: 12px;
    }
    .status { color: var(--muted); min-height: 1.4em; padding: 6px 0 10px; font-size: 0.95rem; }

    /* ── Tree ── */
    .tree {
      border: 1px solid var(--line);
      border-radius: 14px;
      overflow: hidden;
      background: var(--panel);
    }
    .folder-item { border-bottom: 1px solid var(--line); }
    .folder-item:last-child { border-bottom: 0; }

    .folder-row {
      display: flex; align-items: center; gap: 0;
      padding: 10px 14px; background: var(--panel);
      user-select: none;
    }
    .expand-btn {
      background: none; border: 0; border-radius: 6px;
      width: 28px; height: 28px; flex-shrink: 0;
      font-size: 0.75rem; cursor: pointer; color: var(--muted);
      display: flex; align-items: center; justify-content: center;
      transition: background .12s;
    }
    .expand-btn:hover { background: var(--accent-soft); }

    .folder-chk-wrap {
      display: flex; align-items: center; gap: 10px; flex: 1; cursor: pointer;
      padding: 2px 0;
    }
    .folder-chk-wrap input[type="checkbox"] {
      width: 17px; height: 17px; accent-color: var(--accent); flex-shrink: 0; cursor: pointer;
    }
    .folder-name-text { font-weight: 600; font-size: 1rem; }
    .subfolder-badge {
      font-size: 0.78rem; background: var(--accent-soft);
      color: var(--accent); border-radius: 999px; padding: 1px 9px;
    }

    /* ── Subfolder panel ── */
    .subfolders {
      background: #f5f0ea;
      border-top: 1px solid var(--line);
      padding: 10px 14px 12px 46px;
    }
    .sub-actions { font-size: 0.82rem; margin-bottom: 8px; color: var(--muted); }
    .sub-actions button {
      background: none; color: var(--accent); padding: 0 2px;
      font: inherit; font-size: 0.82rem; border-radius: 3px;
      text-decoration: underline; display: inline;
    }
    .sub-actions button:hover { background: var(--accent-soft); }
    .sub-grid {
      display: grid;
      grid-template-columns: repeat(auto-fill, minmax(200px, 1fr));
      gap: 2px 14px;
    }
    .sub-label {
      display: flex; align-items: center; gap: 8px;
      padding: 5px 6px; border-radius: 6px; cursor: pointer; font-size: 0.95rem;
    }
    .sub-label:hover { background: var(--accent-soft); }
    .sub-label input[type="checkbox"] {
      width: 15px; height: 15px; accent-color: var(--accent); flex-shrink: 0; cursor: pointer;
    }
    .sub-label.whole-active { opacity: 0.45; pointer-events: none; }
    .sub-placeholder { color: var(--muted); font-size: 0.9rem; padding: 2px 0; }

    /* ── Select-all header ── */
    .tree-header {
      display: flex; align-items: center; gap: 10px;
      padding: 10px 14px;
      background: #eee8df;
      border-bottom: 1px solid var(--line);
      user-select: none;
    }
    .tree-header input[type="checkbox"] {
      width: 17px; height: 17px; accent-color: var(--accent); flex-shrink: 0; cursor: pointer;
    }
    .tree-header-label { font-size: 0.88rem; color: var(--muted); cursor: pointer; }
  </style>
</head>
<body>
<main>
  <h1>Selective Sync</h1>
  <p class="lede">Check a folder to include it. Click the arrow to expand and pick individual subfolders instead.</p>
  <p class="meta">Remote: <code id="remote"></code> &nbsp;&middot;&nbsp; Filter: <code id="filter-file"></code></p>

  <div class="toolbar">
    <button id="save">Save Selection</button>
    <button id="restart">Restart gclone</button>
    <button id="show-all" class="secondary">Show Entire Drive</button>
    <button id="reload" class="secondary">Reload</button>
  </div>

  <input id="search" class="search-box" type="search" placeholder="Search folders&hellip;">

  <div id="warning" class="warning" hidden></div>
  <div id="restart-note" class="warning" hidden></div>
  <p id="status" class="status"></p>

  <div id="tree" class="tree"></div>
</main>

<script>
  let state = null;

  async function loadState() {
    setStatus("Loading folders…");
    const res = await fetch("/api/state");
    if (!res.ok) throw new Error(await res.text());
    state = await res.json();
    document.getElementById("remote").textContent = state.remote;
    document.getElementById("filter-file").textContent = state.filterFile;
    renderWarning();
    renderRestartNote(false);
    renderTree();
    setStatus("Loaded " + state.tree.length + " top-level folder" + (state.tree.length !== 1 ? "s" : "") + ".");
  }

  function renderWarning() {
    const el = document.getElementById("warning");
    const rules = state.selection.unsupportedRules || [];
    if (!rules.length) { el.hidden = true; return; }
    el.hidden = false;
    el.textContent = "Custom filter rules not managed by this GUI will be replaced on save: " + rules.join(" | ");
  }

  function renderTree() {
    const root = document.getElementById("tree");
    const q = document.getElementById("search").value.trim().toLowerCase();
    root.innerHTML = "";

    const visible = state.tree.filter(function(f) {
      if (!q) return true;
      if (f.name.toLowerCase().indexOf(q) !== -1) return true;
      const subs = state.selection.selectedChildren[f.name] || [];
      return subs.some(function(s) { return s.toLowerCase().indexOf(q) !== -1; });
    });

    if (!visible.length) {
      root.innerHTML = '<div style="padding:18px;color:var(--muted)">No folders match.</div>';
      return;
    }

    const header = document.createElement("div");
    header.className = "tree-header";
    const selectAllChk = document.createElement("input");
    selectAllChk.type = "checkbox";
    selectAllChk.id = "select-all-chk";
    const selectAllLbl = document.createElement("label");
    selectAllLbl.htmlFor = "select-all-chk";
    selectAllLbl.className = "tree-header-label";
    selectAllLbl.textContent = "Select all folders";
    header.appendChild(selectAllChk);
    header.appendChild(selectAllLbl);
    root.appendChild(header);

    selectAllChk.addEventListener("change", function() {
      var target = selectAllChk.checked;
      document.querySelectorAll("input[data-kind='whole']").forEach(function(chk) {
        chk.checked = target;
        chk.dispatchEvent(new Event("change"));
      });
    });

    visible.forEach(function(f) { appendFolderItem(root, f.name, q); });
    updateSelectAll();
  }

  function appendFolderItem(root, name, q) {
    const savedKids = new Set(state.selection.selectedChildren[name] || []);
    const isWhole   = !!state.selection.wholeFolders[name];
    const hasKids   = savedKids.size > 0;

    /* wrapper */
    const item = document.createElement("div");
    item.className = "folder-item";

    /* ── folder row ── */
    const row = document.createElement("div");
    row.className = "folder-row";

    const expandBtn = document.createElement("button");
    expandBtn.className = "expand-btn";
    expandBtn.title = "Expand / collapse";
    expandBtn.textContent = "▶";
    expandBtn.setAttribute("aria-expanded", "false");
    row.appendChild(expandBtn);

    const chkWrap = document.createElement("label");
    chkWrap.className = "folder-chk-wrap";

    const chk = document.createElement("input");
    chk.type = "checkbox";
    chk.dataset.top  = name;
    chk.dataset.kind = "whole";
    chk.checked = isWhole;
    chkWrap.appendChild(chk);

    const nameSpan = document.createElement("span");
    nameSpan.className = "folder-name-text";
    nameSpan.textContent = name;
    chkWrap.appendChild(nameSpan);

    if (hasKids && !isWhole) {
      const badge = document.createElement("span");
      badge.className = "subfolder-badge";
      badge.textContent = savedKids.size + " subfolder" + (savedKids.size > 1 ? "s" : "");
      chkWrap.appendChild(badge);
    }
    row.appendChild(chkWrap);
    item.appendChild(row);

    /* ── subfolder panel ── */
    const panel = document.createElement("div");
    panel.className = "subfolders";
    panel.hidden = true;
    panel.dataset.loaded = "false";
    item.appendChild(panel);

    root.appendChild(item);

    /* expand / collapse */
    function doExpand() {
      if (!panel.hidden) {
        panel.hidden = true;
        expandBtn.textContent = "▶";
        expandBtn.setAttribute("aria-expanded", "false");
        return;
      }
      panel.hidden = false;
      expandBtn.textContent = "▼";
      expandBtn.setAttribute("aria-expanded", "true");
      ensureSubsLoaded(name, panel, savedKids, chk, q);
    }
    expandBtn.addEventListener("click", doExpand);

    /* whole-folder checkbox disables subfolder boxes */
    chk.addEventListener("change", function() {
      syncSubDisabled(panel, chk.checked);
      refreshBadge(chkWrap, panel, chk);
      updateSelectAll();
    });

    /* auto-expand if there are saved subfolder selections */
    if (hasKids) doExpand();
  }

  async function ensureSubsLoaded(name, panel, savedKids, wholeChk, q) {
    if (panel.dataset.loaded === "true") {
      applySubFilter(panel, q, name);
      return;
    }
    panel.innerHTML = '<div class="sub-placeholder">Loading…</div>';
    const res = await fetch("/api/children?top=" + encodeURIComponent(name));
    if (!res.ok) {
      panel.innerHTML = '<div class="sub-placeholder" style="color:red">' + esc(await res.text()) + '</div>';
      return;
    }
    const data = await res.json();
    panel.innerHTML = "";
    panel.dataset.loaded = "true";

    if (!data.children || !data.children.length) {
      panel.innerHTML = '<div class="sub-placeholder">No subfolders.</div>';
      return;
    }

    /* all / none links */
    const actions = document.createElement("div");
    actions.className = "sub-actions";
    const allBtn  = document.createElement("button");
    allBtn.className = "secondary";
    allBtn.textContent = "all";
    const noneBtn = document.createElement("button");
    noneBtn.className = "secondary";
    noneBtn.textContent = "none";
    actions.append("Select: ", allBtn, " / ", noneBtn);
    panel.appendChild(actions);

    const grid = document.createElement("div");
    grid.className = "sub-grid";
    panel.appendChild(grid);

    data.children.forEach(function(child) {
      const lbl = document.createElement("label");
      lbl.className = "sub-label" + (wholeChk.checked ? " whole-active" : "");
      lbl.dataset.child = child.toLowerCase();
      const box = document.createElement("input");
      box.type = "checkbox";
      box.dataset.top   = name;
      box.dataset.child = child;
      box.checked  = savedKids.has(child);
      box.disabled = wholeChk.checked;
      lbl.appendChild(box);
      lbl.append(child);
      box.addEventListener("change", function() {
        const chkWrap = panel.closest(".folder-item").querySelector(".folder-chk-wrap");
        const parentChk = chkWrap.querySelector("input[data-kind='whole']");
        refreshBadge(chkWrap, panel, parentChk);
      });
      grid.appendChild(lbl);
    });

    allBtn.addEventListener("click",  function() {
      grid.querySelectorAll("input[type='checkbox']").forEach(function(b) { if (!b.disabled) b.checked = true; });
      var chkWrap = panel.closest(".folder-item").querySelector(".folder-chk-wrap");
      var parentChk = chkWrap.querySelector("input[data-kind='whole']");
      refreshBadge(chkWrap, panel, parentChk);
    });
    noneBtn.addEventListener("click", function() {
      grid.querySelectorAll("input[type='checkbox']").forEach(function(b) { b.checked = false; });
      var chkWrap = panel.closest(".folder-item").querySelector(".folder-chk-wrap");
      var parentChk = chkWrap.querySelector("input[data-kind='whole']");
      refreshBadge(chkWrap, panel, parentChk);
    });

    applySubFilter(panel, q, name);
  }

  function syncSubDisabled(panel, disabled) {
    panel.querySelectorAll(".sub-label").forEach(function(lbl) {
      lbl.classList.toggle("whole-active", disabled);
      lbl.querySelector("input").disabled = disabled;
    });
  }

  function refreshBadge(chkWrap, panel, parentChk) {
    var existing = chkWrap.querySelector(".subfolder-badge");
    if (parentChk.checked) {
      if (existing) existing.remove();
      return;
    }
    var count = panel.querySelectorAll(".sub-label input:checked").length;
    if (!count) { if (existing) existing.remove(); return; }
    var badge = existing || document.createElement("span");
    badge.className = "subfolder-badge";
    badge.textContent = count + " subfolder" + (count > 1 ? "s" : "");
    if (!existing) chkWrap.appendChild(badge);
  }

  function updateSelectAll() {
    var chk = document.getElementById("select-all-chk");
    if (!chk) return;
    var all = document.querySelectorAll("input[data-kind='whole']");
    var checked = 0;
    all.forEach(function(b) { if (b.checked) checked++; });
    chk.indeterminate = checked > 0 && checked < all.length;
    chk.checked = checked === all.length && all.length > 0;
  }

  function applySubFilter(panel, q, top) {
    if (!q) {
      panel.querySelectorAll(".sub-label").forEach(function(l) { l.hidden = false; });
      return;
    }
    var topMatch = top.toLowerCase().indexOf(q) !== -1;
    panel.querySelectorAll(".sub-label").forEach(function(l) {
      l.hidden = !(topMatch || l.dataset.child.indexOf(q) !== -1);
    });
  }

  function collectPayload(showAll) {
    if (showAll) return { showAll: true, wholeFolders: {}, selectedChildren: {} };
    var wholeFolders = {}, selectedChildren = {};
    document.querySelectorAll("input[data-kind='whole']").forEach(function(chk) {
      var top = chk.dataset.top;
      if (chk.checked) { wholeFolders[top] = true; return; }
      var kids = [];
      document.querySelectorAll("input[data-top='" + cssEsc(top) + "'][data-child]").forEach(function(b) {
        if (b.checked) kids.push(b.dataset.child);
      });
      if (kids.length) selectedChildren[top] = kids;
    });
    return { showAll: false, wholeFolders: wholeFolders, selectedChildren: selectedChildren };
  }

  async function save(showAll) {
    var btn = document.getElementById("save");
    btn.disabled = true;
    try {
      var res = await fetch("/api/save", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(collectPayload(showAll)),
      });
      if (!res.ok) throw new Error(await res.text());
      var data = await res.json();
      renderRestartNote(true);
      setStatus(data.message);
      await loadState();
      renderRestartNote(true);
    } finally { btn.disabled = false; }
  }

  async function restartService() {
    var btn = document.getElementById("restart");
    btn.disabled = true;
    try {
      setStatus("Restarting gclone…");
      var res = await fetch("/api/restart", { method: "POST" });
      if (!res.ok) throw new Error(await res.text());
      var data = await res.json();
      renderRestartNote(false);
      setStatus(data.message + (data.output ? " " + data.output : ""));
    } finally { btn.disabled = false; }
  }

  function renderRestartNote(show) {
    var el = document.getElementById("restart-note");
    if (!show) { el.hidden = true; return; }
    el.hidden = false;
    el.innerHTML = "Selection saved. Click <strong>Restart gclone</strong> to apply, or run: <code>" + esc(state.restartCommand) + "</code>";
  }

  function setStatus(msg) { document.getElementById("status").textContent = msg; }

  function esc(v) {
    return String(v).replaceAll("&","&amp;").replaceAll("<","&lt;").replaceAll(">","&gt;").replaceAll('"',"&quot;");
  }
  function cssEsc(v) { return window.CSS && CSS.escape ? CSS.escape(v) : v.replaceAll('"', '\\"'); }

  document.getElementById("save").addEventListener("click",     function() { save(false).catch(function(e) { setStatus(e.message); }); });
  document.getElementById("restart").addEventListener("click",  function() { restartService().catch(function(e) { setStatus(e.message); }); });
  document.getElementById("show-all").addEventListener("click", function() { save(true).catch(function(e) { setStatus(e.message); }); });
  document.getElementById("reload").addEventListener("click",   function() { loadState().catch(function(e) { setStatus(e.message); }); });
  document.getElementById("search").addEventListener("input",   function() { renderTree(); });

  loadState().catch(function(e) { setStatus(e.message); });
</script>
</body>
</html>`
