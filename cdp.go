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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
func browserWSURL() (string, error) {
	if v := os.Getenv("BU_CDP_WS"); v != "" {
		return v, nil
	}
	if v := os.Getenv("BU_CDP_URL"); v != "" {
		if ws := versionWS(v); ws != "" {
			return ws, nil
		}
		return "", fmt.Errorf("BU_CDP_URL=%s did not return a webSocketDebuggerUrl", v)
	}
	// Scan profile directories for an active debugging port. Prefer the
	// HTTP endpoint (gives a live WS URL); fall back to the file's ws path,
	// which is what the chrome://inspect toggle leaves behind.
	for _, dir := range profileDirs() {
		port, wsPath := activePort(dir)
		if port == "" {
			continue
		}
		if ws := versionWS("http://127.0.0.1:" + port); ws != "" {
			return ws, nil
		}
		if wsPath != "" {
			return "ws://127.0.0.1:" + port + wsPath, nil
		}
	}
	// Last resort: probe the common ports directly.
	for _, port := range []string{"9222", "9223"} {
		if ws := versionWS("http://127.0.0.1:" + port); ws != "" {
			return ws, nil
		}
	}
	return "", fmt.Errorf("no browser debug endpoint found.\n%s\nOr set BU_CDP_URL=http://host:port for a remote endpoint.", connectHelp())
}

func connected() bool {
	_, err := browserWSURL()
	return err == nil
}

// ---- JSON-RPC over the browser WebSocket ----

type cdpClient struct {
	ws        *wsConn
	nextID    int
	sessionID string // attached page session (flattened)
	targetID  string // attached page target id
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
	return c.rpc(method, params, c.sessionID, defaultTimeout)
}

func (c *cdpClient) CallTimeout(method string, params any, timeout time.Duration) (json.RawMessage, error) {
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
	wsURL, err := browserWSURL()
	if err != nil {
		return nil, err
	}
	// In chrome://inspect toggle mode the browser shows a one-time "Allow"
	// popup when a debugger first connects and holds the handshake until the
	// user accepts, so allow generous time for that click.
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
	st := loadState()
	chosen := pages[0]
	for _, p := range pages {
		if p.TargetID == st.Target {
			chosen = p
			break
		}
	}
	if err := c.attach(chosen.TargetID); err != nil {
		c.Close()
		return nil, err
	}
	if st.Target != chosen.TargetID {
		saveState(buState{Target: chosen.TargetID})
	}
	return c, nil
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
}

func stateFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "use-browser", "state.json")
}

func loadState() buState {
	var st buState
	b, err := os.ReadFile(stateFile())
	if err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func saveState(st buState) {
	p := stateFile()
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := json.Marshal(st)
	os.WriteFile(p, b, 0o644)
}
