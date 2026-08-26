package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func normalizeURL(u string) string {
	if !strings.Contains(u, "://") && !strings.HasPrefix(u, "about:") {
		return "https://" + u
	}
	return u
}

func cmdNav(c *cdpClient, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: use-browser nav <url>")
	}
	u := normalizeURL(args[0])
	res, err := c.Call("Page.navigate", map[string]any{"url": u})
	if err != nil {
		return err
	}
	var r struct {
		ErrorText string `json:"errorText"`
	}
	json.Unmarshal(res, &r)
	if r.ErrorText != "" {
		return fmt.Errorf("navigate: %s", r.ErrorText)
	}
	c.waitReady(12 * time.Second)
	info, err := c.evalString(`JSON.stringify({u: location.href.slice(0,300), t: document.title.slice(0,120)})`)
	if err != nil {
		return err
	}
	var it struct{ U, T string }
	json.Unmarshal([]byte(info), &it)
	fmt.Printf("ok %s %q\n", it.U, it.T)
	return nil
}

func cmdOpen(c *cdpClient, args []string) error {
	u := "about:blank"
	if len(args) == 1 {
		u = normalizeURL(args[0])
	} else if len(args) > 1 {
		return fmt.Errorf("usage: use-browser open [url]")
	}
	id, err := c.newPage(u)
	if err != nil {
		return err
	}
	saveTarget(id)
	if err := c.attach(id); err != nil {
		fmt.Printf("ok tab %s\n", id[:8])
		return nil
	}
	if u != "about:blank" {
		// the tab starts at about:blank and then navigates; wait for the URL first
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			href, err := c.evalString("location.href")
			if err == nil && href != "about:blank" && href != "" {
				break
			}
			time.Sleep(120 * time.Millisecond)
		}
	}
	c.waitReady(12 * time.Second)
	info, _ := c.evalString(`JSON.stringify({u: location.href.slice(0,300), t: document.title.slice(0,120)})`)
	var it struct{ U, T string }
	json.Unmarshal([]byte(info), &it)
	fmt.Printf("ok %s %q\n", it.U, it.T)
	return nil
}

func cmdTabs(c *cdpClient, _ []string) error {
	pages, err := c.pageTargets()
	if err != nil {
		return err
	}
	for i, p := range pages {
		mark := " "
		if p.TargetID == c.targetID {
			mark = "*"
		}
		title := p.Title
		if len(title) > 60 {
			title = title[:60] + "…"
		}
		fmt.Printf("%d%s %q %s\n", i+1, mark, title, p.URL)
	}
	return nil
}

func cmdTab(c *cdpClient, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: use-browser tab <n>  (from `use-browser tabs`)")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("usage: use-browser tab <n>")
	}
	pages, err := c.pageTargets()
	if err != nil {
		return err
	}
	if n < 1 || n > len(pages) {
		return fmt.Errorf("tab %d out of range (1-%d)", n, len(pages))
	}
	t := pages[n-1]
	saveTarget(t.TargetID)
	if err := c.attach(t.TargetID); err != nil {
		return err
	}
	c.browserCall("Target.activateTarget", map[string]any{"targetId": t.TargetID})
	fmt.Printf("ok %q %s\n", t.Title, t.URL)
	return nil
}

func cmdClose(c *cdpClient, _ []string) error {
	if _, err := c.browserCall("Target.closeTarget", map[string]any{"targetId": c.targetID}); err != nil {
		return err
	}
	saveTarget("")
	fmt.Println("ok")
	return nil
}

const textJS = `(() => {
let t = document.body ? document.body.innerText : '';
t = t.replace(/\n{3,}/g, '\n\n').replace(/[ \t]{2,}/g, ' ').trim();
return t;
})()`

func cmdText(c *cdpClient, args []string) error {
	max := 4000
	for i := 0; i < len(args); i++ {
		if args[i] == "--max" && i+1 < len(args) {
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return fmt.Errorf("--max: %v", err)
			}
			max = n
			i++
		}
	}
	t, err := c.evalString(textJS)
	if err != nil {
		return err
	}
	if len(t) > max {
		fmt.Println(t[:max])
		fmt.Printf("... (truncated, %d more chars; use --max %d)\n", len(t)-max, len(t))
	} else {
		fmt.Println(t)
	}
	return nil
}

func cmdJS(c *cdpClient, args []string) error {
	var expr string
	if len(args) == 0 || (len(args) == 1 && args[0] == "-") {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		expr = string(b)
	} else {
		expr = strings.Join(args, " ")
	}
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("usage: use-browser js <expression>  (or pipe on stdin)")
	}
	// allow multi-statement input: wrap in an IIFE if it looks like statements
	if strings.Contains(expr, ";") || strings.Contains(expr, "return ") {
		if !strings.Contains(expr, "return") {
			expr = "(() => { " + expr + " })()"
		} else {
			expr = "(() => { " + expr + " })()"
		}
	}
	v, err := c.eval(expr, true)
	if err != nil {
		return err
	}
	if v == nil {
		fmt.Println("undefined")
		return nil
	}
	// print strings raw, everything else as JSON
	var s string
	if json.Unmarshal(v, &s) == nil {
		fmt.Println(s)
	} else {
		fmt.Println(string(v))
	}
	return nil
}

func cmdShot(c *cdpClient, args []string) error {
	full := false
	var path string
	for _, a := range args {
		if a == "--full" {
			full = true
		} else {
			path = a
		}
	}
	if path == "" {
		path = filepath.Join(os.TempDir(), "use-browser-shot.png")
	}
	params := map[string]any{"format": "png"}
	if full {
		params["captureBeyondViewport"] = true
	}
	res, err := c.CallTimeout("Page.captureScreenshot", params, 30*time.Second)
	if err != nil {
		return err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(r.Data)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	abs, _ := filepath.Abs(path)
	fmt.Printf("ok %s (%d KB)\n", abs, len(b)/1024)
	return nil
}

func cmdScroll(c *cdpClient, args []string) error {
	arg := "down"
	if len(args) > 0 {
		arg = args[0]
	}
	var js string
	switch arg {
	case "top":
		js = "scrollTo(0,0)"
	case "bottom":
		js = "scrollTo(0,document.documentElement.scrollHeight)"
	case "down":
		js = "scrollBy(0,Math.round(innerHeight*0.8))"
	case "up":
		js = "scrollBy(0,-Math.round(innerHeight*0.8))"
	default:
		n, err := strconv.Atoi(arg)
		if err != nil {
			return fmt.Errorf("usage: use-browser scroll [down|up|top|bottom|<px>]")
		}
		js = fmt.Sprintf("scrollBy(0,%d)", n)
	}
	if _, err := c.eval(js, false); err != nil {
		return err
	}
	time.Sleep(150 * time.Millisecond)
	pos, err := c.evalString(`JSON.stringify({sy: Math.round(scrollY), h: innerHeight, ph: Math.round(document.documentElement.scrollHeight)})`)
	if err != nil {
		return err
	}
	var p struct{ Sy, H, Ph int }
	json.Unmarshal([]byte(pos), &p)
	fmt.Printf("ok scroll=%d+%d/%d\n", p.Sy, p.H, p.Ph)
	return nil
}

func cmdType(c *cdpClient, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: use-browser type <text>")
	}
	text := strings.Join(args, " ")
	if _, err := c.Call("Input.insertText", map[string]any{"text": text}); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// key names -> (windows virtual key code, DOM code, text)
var keyMap = map[string][3]string{
	"enter": {"13", "Enter", "\r"}, "tab": {"9", "Tab", "\t"}, "backspace": {"8", "Backspace", ""},
	"escape": {"27", "Escape", ""}, "esc": {"27", "Escape", ""}, "delete": {"46", "Delete", ""},
	"space": {"32", "Space", " "}, "left": {"37", "ArrowLeft", ""}, "up": {"38", "ArrowUp", ""},
	"right": {"39", "ArrowRight", ""}, "down": {"40", "ArrowDown", ""}, "home": {"36", "Home", ""},
	"end": {"35", "End", ""}, "pageup": {"33", "PageUp", ""}, "pagedown": {"34", "PageDown", ""},
	"f5": {"116", "F5", ""},
}

var keyCanonical = map[string]string{
	"enter": "Enter", "tab": "Tab", "backspace": "Backspace", "escape": "Escape", "esc": "Escape",
	"delete": "Delete", "space": " ", "left": "ArrowLeft", "up": "ArrowUp", "right": "ArrowRight",
	"down": "ArrowDown", "home": "Home", "end": "End", "pageup": "PageUp", "pagedown": "PageDown", "f5": "F5",
}

// cmdKey: `use-browser key Enter`, `use-browser key ctrl+a`
func cmdKey(c *cdpClient, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: use-browser key <key>  e.g. Enter, Tab, ctrl+a, shift+Tab")
	}
	parts := strings.Split(args[0], "+")
	key := parts[len(parts)-1]
	mods := 0
	for _, m := range parts[:len(parts)-1] {
		switch strings.ToLower(m) {
		case "alt":
			mods |= 1
		case "ctrl", "control":
			mods |= 2
		case "meta", "cmd", "win":
			mods |= 4
		case "shift":
			mods |= 8
		default:
			return fmt.Errorf("unknown modifier %q", m)
		}
	}
	lk := strings.ToLower(key)
	var vk int
	var code, text string
	if m, ok := keyMap[lk]; ok {
		vk, _ = strconv.Atoi(m[0])
		code, text = m[1], m[2]
		key = keyCanonical[lk]
	} else if len(key) == 1 {
		vk = int(strings.ToUpper(key)[0])
		code = key
		text = key
	} else {
		return fmt.Errorf("unknown key %q", key)
	}
	base := map[string]any{
		"key": key, "code": code, "modifiers": mods,
		"windowsVirtualKeyCode": vk, "nativeVirtualKeyCode": vk,
	}
	shortcut := mods&(1|2|4) != 0
	printable := len(key) == 1 && text != "" && !shortcut
	down := map[string]any{"type": "keyDown"}
	for k, v := range base {
		down[k] = v
	}
	if !printable && text != "" {
		down["text"] = text
	}
	if printable {
		down["text"] = text
	}
	if _, err := c.Call("Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	if printable {
		ch := map[string]any{"type": "char", "text": text}
		for k, v := range base {
			ch[k] = v
		}
		if _, err := c.Call("Input.dispatchKeyEvent", ch); err != nil {
			return err
		}
	}
	up := map[string]any{"type": "keyUp"}
	for k, v := range base {
		up[k] = v
	}
	if _, err := c.Call("Input.dispatchKeyEvent", up); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdCDP(c *cdpClient, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: use-browser cdp <Domain.method> [params-json]")
	}
	var params any
	if len(args) > 1 {
		if err := json.Unmarshal([]byte(strings.Join(args[1:], " ")), &params); err != nil {
			return fmt.Errorf("params must be JSON: %v", err)
		}
	}
	res, err := c.Call(args[0], params)
	if err != nil {
		return err
	}
	out := string(res)
	if len(out) > 20000 {
		out = out[:20000] + "\n... (truncated)"
	}
	fmt.Println(out)
	return nil
}

func cmdDoctor(args []string) error {
	// `doctor <browser>` diagnoses that browser specifically, and pins it.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		b, err := findBrowser(strings.ToLower(args[0]))
		if err != nil {
			return err
		}
		pinBrowser(b.Name)
	}
	pin := pinnedBrowser()
	for _, b := range detectBrowsers() {
		run := ""
		if b.isRunning() {
			run = " [running]"
		}
		mark := ""
		if b.Name == pin {
			mark = " <- pinned"
		}
		fmt.Printf("installed: %s (%s)%s%s\n", b.Name, b.Path, run, mark)
	}
	if pin == "" {
		fmt.Println("pinned: none (auto — first running browser wins; use-browser use <name> to pin)")
	} else if os.Getenv("BU_BROWSER") != "" {
		fmt.Printf("pinned: %s (from BU_BROWSER)\n", pin)
	}
	wsURL, err := browserWSURL()
	if err != nil {
		fmt.Println("browser: NOT CONNECTED")
		fmt.Println(err)
		return fmt.Errorf("doctor found problems")
	}
	fmt.Printf("endpoint: %s\n", wsURL)
	c, err := connect()
	if err != nil {
		fmt.Printf("connect: error: %v\n", err)
		return fmt.Errorf("doctor found problems")
	}
	defer c.Close()
	pages, err := c.pageTargets()
	if err != nil {
		fmt.Printf("targets: error: %v\n", err)
		return fmt.Errorf("doctor found problems")
	}
	fmt.Printf("tabs: %d page target(s)\n", len(pages))
	cur := "none"
	for _, p := range pages {
		if p.TargetID == c.targetID {
			cur = fmt.Sprintf("%q %s", p.Title, p.URL)
		}
	}
	fmt.Printf("current tab: %s\n", cur)
	fmt.Printf("state file: %s\n", stateFile())
	return nil
}
