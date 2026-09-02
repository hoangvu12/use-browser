package main

// CDP client. Connects to the browser-level WebSocket and drives pages
// through the Target domain with flattened sessions. This is the model real
// CDP clients use, and it is the only one that works across all connection
// modes:
//
//   - Real profile via the chrome://inspect toggle: the /json/* HTTP API is
//     disabled, but the browser writes its WebSocket path to a
//     DevToolsActivePort file in the profile directory.
//   - Dedicated automation profile (use-browser launch): /json/version and the
//     DevToolsActivePort file both work.
//   - Remote endpoint via BU_CDP_URL / BU_CDP_WS.
//
// No daemon: each invocation dials the browser WS, attaches to the current
// page target, acts, and exits. Cross-invocation state (the current target id)
// lives in a small state file; snapshot element refs live in the page itself
// (window.__bu), so they survive between calls for free.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultTimeout = 15 * time.Second

type target struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
}

var internalPrefixes = []string{"chrome://", "chrome-untrusted://", "devtools://", "chrome-extension://", "edge://", "brave://", "vivaldi://", "opera://"}

func isInternal(u string) bool {
	for _, p := range internalPrefixes {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return false
}

// ---- browser WebSocket discovery ----

// versionWS asks the DevTools HTTP endpoint on a port for the browser WS URL.
// Returns "" when the endpoint is absent or (in toggle mode) returns 404.
func versionWS(base string) string {
	c := http.Client{Timeout: 900 * time.Millisecond}
	resp, err := c.Get(strings.TrimRight(base, "/") + "/json/version")
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.WS
}

// activePort reads a DevToolsActivePort file (line 1: port, line 2: ws path).
func activePort(profileDir string) (port, wsPath string) {
	b, err := os.ReadFile(filepath.Join(profileDir, "DevToolsActivePort"))
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	if len(lines) >= 1 {
		port = strings.TrimSpace(lines[0])
	}
	if len(lines) >= 2 {
		wsPath = strings.TrimSpace(lines[1])
	}
	return port, wsPath
}

// browserWSURL resolves the browser-level WebSocket URL across every mode.
// endpoint is a resolved DevTools connection and the route we took to it.
type endpoint struct {
	ws      string
	browser string
	mode    string // launch | clone | toggle | port | remote
}

// dirMode classifies a profile directory: our own dedicated profile, our copy
// of the user's profile, or the user's real profile driven through the
// <browser>://inspect toggle.
func dirMode(d profileDir) string {
	if strings.HasSuffix(d.path, "-clone") {
		return "clone"
	}
	if d.browser != "" && userDataDirs()[d.browser] == d.path {
		return "toggle"
	}
	return "launch"
}

// ownInstanceEndpoint is a live endpoint for a browser we started ourselves:
// the dedicated launch profile, the clone of the real profile, or the port we
// remembered. It deliberately ignores the user's real profile behind the
// inspect toggle. Adopting that instead of launching is how every later
// command ends up parked on an "Allow remote debugging?" popup.
func ownInstanceEndpoint(name string) string {
	if p := loadState().Port; p != 0 {
		if ws := versionWS("http://127.0.0.1:" + strconv.Itoa(p)); ws != "" {
			return ws
		}
	}
	for _, dir := range []string{launchProfileDir(name), cloneProfileDir(name)} {
		// Our own port file first: it is the only record a second session has
		// of the port a shared clone was launched on.
		if p := readPortFile(dir); p != "" {
			if ws := versionWS("http://127.0.0.1:" + p); ws != "" {
				return ws
			}
		}
		port, wsPath := activePort(dir)
		if port == "" {
			continue
		}
		if ws := versionWS("http://127.0.0.1:" + port); ws != "" {
			return ws
		}
		if wsPath != "" && portAlive(port) {
			return "ws://127.0.0.1:" + port + wsPath
		}
	}
	return ""
}

func browserWSURL() (string, error) {
	e, err := discover()
	return e.ws, err
}

func discover() (endpoint, error) {
	if v := os.Getenv("BU_CDP_WS"); v != "" {
		return endpoint{ws: v, mode: "remote"}, nil
	}
	if v := os.Getenv("BU_CDP_URL"); v != "" {
		if ws := versionWS(v); ws != "" {
			return endpoint{ws: ws, mode: "remote"}, nil
		}
		return endpoint{}, fmt.Errorf("BU_CDP_URL=%s did not return a webSocketDebuggerUrl", v)
	}
	// The port we launched the pinned browser on. Chrome sometimes serves
	// DevTools without ever writing a DevToolsActivePort file, so this is
	// checked before the directory scan.
	if p := loadState().Port; p != 0 {
		if ws := versionWS("http://127.0.0.1:" + strconv.Itoa(p)); ws != "" {
			return endpoint{ws: ws, browser: pinnedBrowser(), mode: "port"}, nil
		}
	}
	// Scan profile directories for an active debugging port. Prefer the
	// HTTP endpoint (gives a live WS URL); fall back to the file's ws path,
	// which is what the chrome://inspect toggle leaves behind.
	pin := pinnedBrowser()
	var found []endpoint
	for _, d := range profileDirs() {
		// Our own port file first. Chrome and Brave often never write
		// DevToolsActivePort, and a clone shared between sessions has no
		// other record of the port it was launched on.
		ws := ""
		if p := readPortFile(d.path); p != "" {
			ws = versionWS("http://127.0.0.1:" + p)
		}
		if ws == "" {
			port, wsPath := activePort(d.path)
			if port == "" {
				continue
			}
			ws = versionWS("http://127.0.0.1:" + port)
			// The ws-path fallback (inspect-toggle mode) only counts if
			// something is actually listening; a DevToolsActivePort file left
			// behind by a closed browser is stale and must not fake a
			// connection.
			if ws == "" && wsPath != "" && portAlive(port) {
				ws = "ws://127.0.0.1:" + port + wsPath
			}
		}
		if ws == "" {
			continue
		}
		e := endpoint{ws: ws, browser: d.browser, mode: dirMode(d)}
		if pin != "" {
			return e, nil // profileDirs already narrowed this to the pin
		}
		found = append(found, e)
	}
	if len(found) > 0 {
		if names := distinctBrowsers(found); len(names) > 1 {
			return endpoint{}, ambiguousBrowsers(names)
		}
		return found[0], nil
	}
	// Last resort: probe the common ports directly. A named session is an
	// explicit "I own my own browser", and this probe cannot tell one Chrome
	// from another, so it would hand session B the browser session A launched.
	if flagSession != "" {
		return endpoint{}, fmt.Errorf("session %q has no browser yet.%srun: use-browser --session %s launch chrome   (or clone / connect)%sOr set BU_CDP_URL=http://host:port to share an existing one.", flagSession, "\n", flagSession, "\n")
	}
	for _, port := range []string{"9222", "9223", "9224"} {
		if pin != "" && !portOwnedBy(port, pin) {
			continue
		}
		if ws := versionWS("http://127.0.0.1:" + port); ws != "" {
			return endpoint{ws: ws, browser: pin, mode: "port"}, nil
		}
	}
	return endpoint{}, fmt.Errorf("no browser debug endpoint found.\n%s\nOr set BU_CDP_URL=http://host:port for a remote endpoint.", connectHelp())
}

// distinctBrowsers lists the browsers behind a set of live endpoints, in
// browserOrder. An unattributable directory counts as its own name so it can
// never be silently merged with a real browser.
func distinctBrowsers(found []endpoint) []string {
	seen := map[string]bool{}
	for _, f := range found {
		name := f.browser
		if name == "" {
			name = "unknown"
		}
		seen[name] = true
	}
	var names []string
	for _, n := range browserOrder {
		if seen[n] {
			names = append(names, n)
			delete(seen, n)
		}
	}
	for n := range seen {
		names = append(names, n)
	}
	return names
}

// ambiguousBrowsers refuses to guess. Picking one here is how an agent ends up
// driving the Brave the user asked it to leave alone.
func ambiguousBrowsers(names []string) error {
	list, verb := strings.Join(names, ", "), "all accept"
	if len(names) == 2 {
		list, verb = names[0]+" and "+names[1], "both accept"
	}
	return fmt.Errorf("%s %s a debug connection, and no browser is pinned.%sPin one — it persists, so you only say it once:%s  use-browser use %s%sOr override a single command: use-browser --browser %s <command>",
		list, verb, "\n", "\n", names[0], "\n", names[0])
}

func connected() bool {
	_, err := browserWSURL()
	return err == nil
}

// portAlive reports whether something is listening on a local TCP port.
func portAlive(port string) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ---- JSON-RPC over the browser WebSocket ----

type cdpClient struct {
	ws        *wsConn
	nextID    int
	sessionID string // attached page session (flattened)
	targetID  string // attached page target id
	// stale holds the remembered target id when it no longer exists. The
	// client is attached to an arbitrary page so `tabs`/`tab` can run, but
	// every page-level command refuses until a tab is picked again.
	stale string
}

// checkTab fails when the client is only provisionally attached.
func (c *cdpClient) checkTab() error {
	if c.stale == "" {
		return nil
	}
	return fmt.Errorf("current tab %s is gone (closed, or it belongs to another browser).%srun: use-browser tabs   then: use-browser tab <id>", shortID(c.stale), "\n")
}

type cdpResp struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Method string `json:"method"`
}

// rpc sends one command and waits for its response. When sessionID is set the
// command is routed to that page session; otherwise it is browser-level.
func (c *cdpClient) rpc(method string, params any, sessionID string, timeout time.Duration) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	if params == nil {
		params = map[string]any{}
	}
	obj := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		obj["sessionId"] = sessionID
	}
	req, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if err := c.ws.WriteText(req); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		msg, err := c.ws.ReadMessage(deadline)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", method, err)
		}
		var r cdpResp
		if json.Unmarshal(msg, &r) != nil {
			continue
		}
		if r.Method != "" { // event
			continue
		}
		if r.ID != id {
			continue
		}
		if r.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, r.Error.Message)
		}
		return r.Result, nil
	}
}

// Call routes to the current page session.
func (c *cdpClient) Call(method string, params any) (json.RawMessage, error) {
	if err := c.checkTab(); err != nil {
		return nil, err
	}
	return c.rpc(method, params, c.sessionID, defaultTimeout)
}

func (c *cdpClient) CallTimeout(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if err := c.checkTab(); err != nil {
		return nil, err
	}
	return c.rpc(method, params, c.sessionID, timeout)
}

// browserCall is a browser-level command (Target domain, no session).
func (c *cdpClient) browserCall(method string, params any) (json.RawMessage, error) {
	return c.rpc(method, params, "", defaultTimeout)
}

func (c *cdpClient) pageTargets() ([]target, error) {
	res, err := c.browserCall("Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		TargetInfos []target `json:"targetInfos"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	var pages []target
	for _, t := range r.TargetInfos {
		if t.Type == "page" && !isInternal(t.URL) {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

func (c *cdpClient) attach(targetID string) error {
	res, err := c.browserCall("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true})
	if err != nil {
		return err
	}
	var r struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(res, &r)
	if r.SessionID == "" {
		return fmt.Errorf("attach %s: no session id returned", targetID)
	}
	c.sessionID = r.SessionID
	c.targetID = targetID
	c.stale = ""
	return nil
}

func (c *cdpClient) newPage(url string) (string, error) {
	res, err := c.browserCall("Target.createTarget", map[string]any{"url": url})
	if err != nil {
		return "", err
	}
	var r struct {
		TargetID string `json:"targetId"`
	}
	json.Unmarshal(res, &r)
	if r.TargetID == "" {
		return "", fmt.Errorf("createTarget: no target id returned")
	}
	return r.TargetID, nil
}

// connect dials the browser WS and attaches to the current or first page.
func connect() (*cdpClient, error) {
	e, err := discover()
	if err != nil {
		return nil, err
	}
	wsURL := e.ws
	// Toggle mode prompts the user on every connection, and this dial is
	// about to raise that dialog. Say so first: a dialog nobody expected,
	// on the browser they are actually using, is not acceptable surprise.
	if e.mode == "toggle" {
		fmt.Fprintf(os.Stderr, "note: connecting to your real %s through its inspect toggle.%s"+
			"      it will ask you to allow this connection, and will ask again next time.%s"+
			"      to stop that: use-browser clone %s   (same logins, no prompt)%s",
			e.browser, "\n", "\n", e.browser, "\n")
	}
	// The browser holds the handshake open until the user answers that
	// dialog, so allow generous time for the click.
	dialTimeout := 8 * time.Second
	if os.Getenv("BU_CDP_WS") == "" && os.Getenv("BU_CDP_URL") == "" {
		dialTimeout = 60 * time.Second
	}
	ws, err := wsDial(wsURL, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("browser websocket %s: %v", wsURL, err)
	}
	c := &cdpClient{ws: ws}
	pages, err := c.pageTargets()
	if err != nil {
		c.Close()
		return nil, err
	}
	if len(pages) == 0 {
		id, err := c.newPage("about:blank")
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("no open tabs and could not create one: %v", err)
		}
		pages = []target{{TargetID: id, Type: "page", URL: "about:blank"}}
	}
	// --tab addresses a tab directly for this one invocation: no state read,
	// no state write. This is the race-free way for several agents to drive
	// several tabs of the same browser at once.
	if flagTab != "" {
		t, err := resolveTabID(pages, flagTab)
		if err != nil {
			c.Close()
			return nil, err
		}
		if err := c.attach(t.TargetID); err != nil {
			c.Close()
			return nil, err
		}
		return c, nil
	}
	st := loadState()
	if st.Target != "" {
		for _, p := range pages {
			if p.TargetID == st.Target {
				if err := c.attach(p.TargetID); err != nil {
					c.Close()
					return nil, err
				}
				return c, nil
			}
		}
		// The remembered tab is gone. Silently adopting some other tab is how
		// one agent ends up driving another agent's page, so attach only far
		// enough for `tabs` and `tab` to run and let checkTab stop the rest.
		if err := c.attach(pages[0].TargetID); err != nil {
			c.Close()
			return nil, err
		}
		c.stale = st.Target
		return c, nil
	}
	// No tab picked yet: adopt one and remember it.
	if err := c.attach(pages[0].TargetID); err != nil {
		c.Close()
		return nil, err
	}
	saveTarget(pages[0].TargetID)
	return c, nil
}

// shortID is the 8-character prefix used to address a tab on the command line.
// Target ids are 32 hex chars; 8 is plenty to stay unique within one browser
// and short enough for an agent to copy around.
func shortID(id string) string {
	if len(id) > 8 {
		id = id[:8]
	}
	return strings.ToLower(id)
}

// resolveTabID matches a target-id prefix (min 4 chars) against the open
// pages. Indexes are deliberately not accepted here: --tab exists precisely
// because positional indexes are unstable.
func resolveTabID(pages []target, ref string) (*target, error) {
	if len(ref) < 4 {
		return nil, fmt.Errorf("tab id %q is too short (use at least 4 characters from `use-browser tabs`)", ref)
	}
	var hits []*target
	for i := range pages {
		if strings.HasPrefix(pages[i].TargetID, strings.ToUpper(ref)) || strings.HasPrefix(pages[i].TargetID, ref) {
			hits = append(hits, &pages[i])
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return nil, fmt.Errorf("no tab with id %q (run: use-browser tabs)", ref)
	default:
		return nil, fmt.Errorf("tab id %q is ambiguous, matches %d tabs (use more characters)", ref, len(hits))
	}
}

// resolveTab accepts either a 1-based index from `use-browser tabs` or a
// target-id prefix. A short run of digits is an index; anything else is an id.
// Target ids are 32 hex chars, so the two never collide in practice.
func resolveTab(pages []target, ref string) (*target, error) {
	if n, err := strconv.Atoi(ref); err == nil && len(ref) < 8 {
		if n < 1 || n > len(pages) {
			return nil, fmt.Errorf("tab %d out of range (1-%d)", n, len(pages))
		}
		return &pages[n-1], nil
	}
	return resolveTabID(pages, ref)
}

func (c *cdpClient) Close() {
	if c.ws != nil {
		c.ws.Close()
	}
}

// eval runs a JS expression in the attached page, returning the JSON value.
func (c *cdpClient) eval(expr string, awaitPromise bool) (json.RawMessage, error) {
	res, err := c.Call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
		"awaitPromise":  awaitPromise,
	})
	if err != nil {
		return nil, err
	}
	var r struct {
		Result struct {
			Value    json.RawMessage `json:"value"`
			Subtype  string          `json:"subtype"`
			Unserial string          `json:"unserializableValue"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return nil, err
	}
	if r.ExceptionDetails != nil {
		msg := r.ExceptionDetails.Text
		if r.ExceptionDetails.Exception != nil && r.ExceptionDetails.Exception.Description != "" {
			msg = r.ExceptionDetails.Exception.Description
		}
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i] // first line of the JS stack is enough
		}
		return nil, fmt.Errorf("js: %s", msg)
	}
	if r.Result.Unserial != "" {
		return json.RawMessage(`"` + r.Result.Unserial + `"`), nil
	}
	return r.Result.Value, nil
}

// evalString runs JS that returns a string and decodes it.
func (c *cdpClient) evalString(expr string) (string, error) {
	v, err := c.eval(expr, false)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return string(v), nil
	}
	return s, nil
}

// waitReady polls document.readyState until complete (or timeout — not fatal).
func (c *cdpClient) waitReady(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := c.evalString("document.readyState")
		if err == nil && s == "complete" {
			return
		}
		time.Sleep(120 * time.Millisecond)
	}
}

// ---- state file ----

type buState struct {
	Target string `json:"target"`
	// Browser pins every command to one browser (see: use-browser use).
	// Empty means "whichever is running", the old behaviour.
	Browser string `json:"browser,omitempty"`
	// Port is the debug port we last launched the pinned browser on. Chrome
	// does not always write a DevToolsActivePort file, so remembering the
	// port we chose is the only reliable way back to that instance.
	Port int `json:"port,omitempty"`
}

// stateDir is where the state file(s) and launch profiles live.
// stateDir holds the state file(s) and the automation profiles. It defaults to
// the OS cache directory; BU_HOME moves the lot somewhere else, which matters
// because cloned profiles are large and hold real cookies.
func stateDir() string {
	if v := strings.TrimSpace(os.Getenv("BU_HOME")); v != "" {
		return v
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "use-browser")
}

// stateFile is the current tab / pinned browser / debug port for this session.
// Without a session that is one shared slot, which is fine for a single agent
// and wrong for several: BU_SESSION (or --session) gives each agent its own.
func stateFile() string {
	name := "state.json"
	if flagSession != "" {
		name = "state-" + flagSession + ".json"
	}
	return filepath.Join(stateDir(), name)
}

// readState parses one state file. A missing file is not an error: it just
// means nothing has been chosen yet.
func readState(path string) (buState, error) {
	var st buState
	b, err := os.ReadFile(path)
	if err != nil {
		return st, nil
	}
	// PowerShell's Set-Content, Out-File and > all write a UTF-8 BOM by
	// default. Without this the parse fails and the pin, the port and the
	// current tab all vanish with no message.
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if err := json.Unmarshal(b, &st); err != nil {
		return buState{}, fmt.Errorf("state file %s is not valid JSON: %v", path, err)
	}
	return st, nil
}

var warnedBadState bool

func loadState() buState {
	st, err := readState(stateFile())
	if err != nil {
		if !warnedBadState {
			warnedBadState = true
			fmt.Fprintf(os.Stderr, "warning: %v%swarning: ignoring the browser pin and current tab; fix or delete that file%s", err, "\n", "\n")
		}
		return buState{}
	}
	// A session keeps its own tab and port, but inherits the browser pin, so
	// `use-browser use chrome` still has to be said only once.
	if flagSession != "" && st.Browser == "" {
		if def, err := readState(filepath.Join(stateDir(), "state.json")); err == nil {
			st.Browser = def.Browser
		}
	}
	return st
}

func saveState(st buState) {
	p := stateFile()
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(st)
	os.WriteFile(p, b, 0o644)
}

// saveTarget updates the current tab without clobbering the pinned browser.
// Under --tab the invocation is stateless by contract, so it writes nothing.
func saveTarget(id string) {
	if flagTab != "" {
		return
	}
	st := loadState()
	st.Target = id
	saveState(st)
}

// savePort remembers the debug port we launched the pinned browser on.
func savePort(port int) {
	st := loadState()
	st.Port = port
	saveState(st)
}
