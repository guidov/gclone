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
	cmd := exec.Command(a.restartCmd[0], a.restartCmd[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		} else {
			msg += "\n" + err.Error()
		}
		http.Error(w, msg, http.StatusBadGateway)
		return
	}
	writeJSON(w, restartResponse{
		Message: "gclone restart completed.",
		Output:  strings.TrimSpace(string(out)),
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
		if line == "- *" {
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

	rules = append(rules, "- *", "")
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
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: Georgia, "Times New Roman", serif;
      background:
        radial-gradient(circle at top left, #fef8ef 0, #f4efe8 42%, #eee5d9 100%);
      color: var(--ink);
    }
    main {
      max-width: 980px;
      margin: 0 auto;
      padding: 32px 20px 80px;
    }
    h1 {
      margin: 0 0 8px;
      font-size: clamp(2rem, 4vw, 3.25rem);
      line-height: 1;
    }
    .lede {
      color: var(--muted);
      max-width: 64ch;
      margin-bottom: 24px;
    }
    .panel {
      background: var(--panel);
      border: 1px solid var(--line);
      border-radius: 18px;
      padding: 20px;
      box-shadow: 0 8px 30px rgba(60, 45, 20, 0.06);
      margin-bottom: 18px;
    }
    .meta {
      display: grid;
      gap: 6px;
      color: var(--muted);
      font-size: 0.95rem;
    }
    .toolbar {
      display: flex;
      flex-wrap: wrap;
      gap: 12px;
      margin-bottom: 16px;
    }
    button {
      border: 0;
      border-radius: 999px;
      padding: 12px 18px;
      background: var(--accent);
      color: white;
      font: inherit;
      cursor: pointer;
    }
    button.secondary {
      background: #e9ddcf;
      color: var(--ink);
    }
    button:disabled {
      opacity: 0.6;
      cursor: wait;
    }
    .warning {
      background: #fff1cf;
      color: var(--warn);
      border-radius: 12px;
      padding: 12px 14px;
      margin-bottom: 16px;
    }
    .folder {
      border-top: 1px solid var(--line);
      padding: 14px 0;
    }
    .folder:first-child {
      border-top: 0;
      padding-top: 0;
    }
    .folder-head {
      display: flex;
      justify-content: space-between;
      gap: 12px;
      align-items: center;
      margin-bottom: 10px;
    }
    .folder-name {
      font-weight: 700;
      font-size: 1.05rem;
    }
    .folder-actions {
      display: flex;
      gap: 16px;
      flex-wrap: wrap;
      color: var(--muted);
    }
    .children {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(240px, 1fr));
      gap: 8px 16px;
      padding: 12px 14px;
      border-radius: 14px;
      background: var(--accent-soft);
    }
    label {
      display: inline-flex;
      gap: 10px;
      align-items: flex-start;
    }
    input[type="checkbox"] {
      margin-top: 3px;
      accent-color: var(--accent);
    }
    .status {
      min-height: 1.5em;
      color: var(--muted);
    }
    .search {
      margin-bottom: 16px;
    }
    .search input {
      width: 100%;
      padding: 12px 14px;
      border: 1px solid var(--line);
      border-radius: 999px;
      background: #fffdf8;
      font: inherit;
    }
    code {
      font-family: "SFMono-Regular", Consolas, monospace;
      font-size: 0.92em;
    }
  </style>
</head>
<body>
  <main>
    <h1>Selective Sync</h1>
    <p class="lede">Choose which top-level folders and subfolders should appear in the mounted Drive view. Saving writes a filter file for the gclone mount. Restart the service after saving.</p>

    <section class="panel">
      <div class="meta">
        <div>Remote: <code id="remote"></code></div>
        <div>Filter file: <code id="filter-file"></code></div>
      </div>
    </section>

    <section class="panel">
      <div class="toolbar">
        <button id="save">Save Selection</button>
        <button id="restart">Restart gclone</button>
        <button id="show-all" class="secondary">Show Entire Drive</button>
        <button id="reload" class="secondary">Reload</button>
      </div>
      <div class="search">
        <input id="search" type="search" placeholder="Filter folders or subfolders">
      </div>
      <div id="warning" class="warning" hidden></div>
      <div id="folders"></div>
      <div id="restart-note" class="warning" hidden></div>
      <p class="status" id="status"></p>
    </section>
  </main>

  <script>
    let state = null;

    async function loadState() {
      setStatus("Loading folders...");
      const res = await fetch("/api/state");
      if (!res.ok) {
        throw new Error(await res.text());
      }
      state = await res.json();
      document.getElementById("remote").textContent = state.remote;
      document.getElementById("filter-file").textContent = state.filterFile;
      renderWarning();
      renderRestartNote(false);
      renderFolders();
      setStatus("Loaded.");
    }

    function renderWarning() {
      const el = document.getElementById("warning");
      const rules = state.selection.unsupportedRules || [];
      if (!rules.length) {
        el.hidden = true;
        el.textContent = "";
        return;
      }
      el.hidden = false;
      el.textContent = "The current filter file contains custom rules this GUI does not fully model. Saving will replace them: " + rules.join(" | ");
    }

    function renderFolders() {
      const root = document.getElementById("folders");
      const query = document.getElementById("search").value.trim().toLowerCase();
      root.innerHTML = "";

      for (const folder of state.tree) {
        const selectedChildren = new Set(state.selection.selectedChildren[folder.name] || []);
        if (query && !folder.name.toLowerCase().includes(query)) {
          let selectedChildMatches = false;
          for (const child of selectedChildren) {
            if (child.toLowerCase().includes(query)) {
              selectedChildMatches = true;
              break;
            }
          }
          if (!selectedChildMatches) {
            continue;
          }
        }

        const wrapper = document.createElement("section");
        wrapper.className = "folder";

        const wholeChecked = !!state.selection.wholeFolders[folder.name];

        const head = document.createElement("div");
        head.className = "folder-head";
        head.innerHTML =
          '<div class="folder-name">' + escapeHtml(folder.name) + '</div>' +
          '<div class="folder-actions">' +
            '<button type="button" class="secondary" data-action="load" data-top="' + escapeAttr(folder.name) + '">Load subfolders</button>' +
            '<button type="button" class="secondary" data-action="all" data-top="' + escapeAttr(folder.name) + '">Select all subfolders</button>' +
            '<button type="button" class="secondary" data-action="none" data-top="' + escapeAttr(folder.name) + '">Clear subfolders</button>' +
            '<label><input type="checkbox" data-top="' + escapeAttr(folder.name) + '" data-kind="whole"> Include entire folder</label>' +
          '</div>';
        wrapper.appendChild(head);

        const wholeBox = head.querySelector('input[data-kind="whole"]');
        wholeBox.checked = wholeChecked;
        const loadButton = head.querySelector('button[data-action="load"]');
        const allButton = head.querySelector('button[data-action="all"]');
        const noneButton = head.querySelector('button[data-action="none"]');

        const children = document.createElement("div");
        children.className = "children";
        children.dataset.loaded = "false";
        children.dataset.top = folder.name;
        children.innerHTML = "<div>Subfolders load on demand.</div>";

        wholeBox.addEventListener("change", () => {
          for (const box of children.querySelectorAll("input[type=checkbox]")) {
            box.disabled = wholeBox.checked;
          }
          allButton.disabled = wholeBox.checked;
          noneButton.disabled = wholeBox.checked;
        });

        loadButton.addEventListener("click", () => loadChildren(folder.name, children, selectedChildren, wholeBox));
        allButton.addEventListener("click", async () => {
          await loadChildren(folder.name, children, selectedChildren, wholeBox);
          for (const box of children.querySelectorAll('input[data-child]')) {
            if (!box.disabled) {
              box.checked = true;
            }
          }
          applyChildFilter(folder.name, children);
        });
        noneButton.addEventListener("click", async () => {
          await loadChildren(folder.name, children, selectedChildren, wholeBox);
          for (const box of children.querySelectorAll('input[data-child]')) {
            box.checked = false;
          }
          applyChildFilter(folder.name, children);
        });

        if (selectedChildren.size > 0) {
          loadChildren(folder.name, children, selectedChildren, wholeBox);
        }

        wrapper.appendChild(children);
        root.appendChild(wrapper);
      }
    }

    async function loadChildren(top, container, selectedChildren, wholeBox) {
      if (container.dataset.loaded === "true") {
        applyChildFilter(top, container);
        return;
      }
      container.innerHTML = "<div>Loading subfolders...</div>";
      const res = await fetch("/api/children?top=" + encodeURIComponent(top));
      if (!res.ok) {
        container.innerHTML = "<div>" + escapeHtml(await res.text()) + "</div>";
        return;
      }
      const data = await res.json();
      container.innerHTML = "";
      container.dataset.loaded = "true";

      if (!data.children.length) {
        container.innerHTML = "<div>No subfolders found.</div>";
        return;
      }

      for (const child of data.children) {
        const row = document.createElement("label");
        row.dataset.child = child.toLowerCase();
        row.innerHTML = '<input type="checkbox" data-top="' + escapeAttr(top) + '" data-child="' + escapeAttr(child) + '"> ' + escapeHtml(child);
        const box = row.querySelector("input");
        box.checked = selectedChildren.has(child);
        box.disabled = wholeBox.checked;
        container.appendChild(row);
      }
      applyChildFilter(top, container);
    }

    function applyChildFilter(top, container) {
      const query = document.getElementById("search").value.trim().toLowerCase();
      if (!query) {
        for (const row of container.querySelectorAll("label[data-child]")) {
          row.hidden = false;
        }
        return;
      }
      const topMatches = top.toLowerCase().includes(query);
      for (const row of container.querySelectorAll("label[data-child]")) {
        row.hidden = !(topMatches || row.dataset.child.includes(query));
      }
    }

    function collectPayload(showAll) {
      if (showAll) {
        return { showAll: true, wholeFolders: {}, selectedChildren: {} };
      }

      const wholeFolders = {};
      const selectedChildren = {};

      for (const wholeBox of document.querySelectorAll('input[data-kind="whole"]')) {
        const top = wholeBox.dataset.top;
        if (wholeBox.checked) {
          wholeFolders[top] = true;
          continue;
        }

        const children = [];
        for (const childBox of document.querySelectorAll('input[data-top="' + cssEscape(top) + '"][data-child]')) {
          if (childBox.checked) {
            children.push(childBox.dataset.child);
          }
        }
        if (children.length) {
          selectedChildren[top] = children;
        }
      }

      return { showAll: false, wholeFolders, selectedChildren };
    }

    async function save(showAll) {
      const btn = document.getElementById("save");
      btn.disabled = true;
      try {
        const res = await fetch("/api/save", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(collectPayload(showAll)),
        });
        if (!res.ok) {
          throw new Error(await res.text());
        }
        const data = await res.json();
        renderRestartNote(true);
        setStatus(data.message);
        await loadState();
        renderRestartNote(true);
      } finally {
        btn.disabled = false;
      }
    }

    async function restartService() {
      const btn = document.getElementById("restart");
      btn.disabled = true;
      try {
        setStatus("Restarting gclone...");
        const res = await fetch("/api/restart", { method: "POST" });
        if (!res.ok) {
          throw new Error(await res.text());
        }
        const data = await res.json();
        renderRestartNote(false);
        setStatus(data.message + (data.output ? " " + data.output : ""));
      } finally {
        btn.disabled = false;
      }
    }

    function renderRestartNote(show) {
      const el = document.getElementById("restart-note");
      if (!show) {
        el.hidden = true;
        el.textContent = "";
        return;
      }
      el.hidden = false;
      el.innerHTML = 'Selection saved. Restart the mount service to apply it: <code>' + escapeHtml(state.restartCommand) + '</code>';
    }

    function setStatus(message) {
      document.getElementById("status").textContent = message;
    }

    function escapeHtml(value) {
      return value
        .replaceAll("&", "&amp;")
        .replaceAll("<", "&lt;")
        .replaceAll(">", "&gt;")
        .replaceAll('"', "&quot;");
    }

    function escapeAttr(value) {
      return escapeHtml(value).replaceAll("'", "&#39;");
    }

    function cssEscape(value) {
      return window.CSS && CSS.escape ? CSS.escape(value) : value.replaceAll('"', '\\"');
    }

    document.getElementById("save").addEventListener("click", () => save(false));
    document.getElementById("restart").addEventListener("click", () => restartService().catch(err => setStatus(err.message)));
    document.getElementById("show-all").addEventListener("click", () => save(true));
    document.getElementById("reload").addEventListener("click", () => loadState().catch(err => setStatus(err.message)));
    document.getElementById("search").addEventListener("input", () => renderFolders());

    loadState().catch(err => setStatus(err.message));
  </script>
</body>
</html>`
