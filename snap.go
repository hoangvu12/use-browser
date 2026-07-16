package main

// snap: one injected JS pass over the DOM (incl. open shadow roots and
// same-origin iframes) that collects visible interactive elements as compact
// indexed lines. Element references are parked in the page itself
// (window.__bu), so `click 5` in a later CLI invocation resolves the live
// element — no re-walk, no coordinate staleness, and it breaks loudly (with
// a "run snap" hint) if the page navigated.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const snapJS = `(() => {
const out = [], refs = [];
const ROLES = new Set(['button','link','tab','menuitem','menuitemcheckbox','menuitemradio','checkbox','radio','combobox','listbox','switch','option','searchbox','textbox','slider','spinbutton']);
function inter(e) {
  const t = e.tagName;
  if (t === 'A') return e.hasAttribute('href');
  if (t === 'BUTTON' || t === 'SELECT' || t === 'TEXTAREA' || t === 'SUMMARY') return true;
  if (t === 'INPUT') return e.type !== 'hidden';
  const r = e.getAttribute('role');
  if (r && ROLES.has(r)) return true;
  if (e.hasAttribute('onclick') || e.hasAttribute('jsaction')) return true;
  if (e.isContentEditable && !(e.parentElement && e.parentElement.isContentEditable)) return true;
  return false;
}
function lbl(e) {
  let t = '';
  if (e.tagName === 'INPUT' || e.tagName === 'TEXTAREA') t = e.value || e.placeholder || '';
  if (e.tagName === 'SELECT' && e.selectedOptions.length) t = e.selectedOptions[0].text;
  if (!t) t = (e.innerText || '').trim();
  if (!t) t = e.getAttribute('aria-label') || e.title || e.alt || e.name || '';
  if (!t && e.tagName === 'A') t = e.getAttribute('href') || '';
  return String(t).trim().replace(/\s+/g, ' ').slice(0, 80);
}
function push(e) {
  // dedupe obvious nesting noise: span inside button, img inside a, ...
  if (e.parentElement && e.parentElement.closest('a,button,select,summary,[role="button"],[role="link"]')) return;
  const r = e.getBoundingClientRect();
  if (r.width < 1 || r.height < 1) return;
  const s = e.ownerDocument.defaultView.getComputedStyle(e);
  if (s.visibility === 'hidden' || s.display === 'none' || +s.opacity === 0) return;
  let d = '<' + e.tagName.toLowerCase();
  if (e.tagName === 'INPUT') d += ':' + (e.type || 'text');
  const role = e.getAttribute('role');
  if (role && e.tagName !== 'A' && e.tagName !== 'BUTTON') d += ' role=' + role;
  d += '>';
  let extra = '';
  if (e.tagName === 'INPUT' && (e.type === 'checkbox' || e.type === 'radio')) extra = e.checked ? ' [x]' : ' [ ]';
  if (e.disabled) extra += ' disabled';
  refs.push(e);
  out.push('[' + refs.length + ']' + d + ' ' + JSON.stringify(lbl(e)) + extra);
}
function walk(root) {
  for (const e of root.querySelectorAll('*')) {
    if (e.shadowRoot) walk(e.shadowRoot);
    if (e.tagName === 'IFRAME' || e.tagName === 'FRAME') {
      try { if (e.contentDocument && e.contentDocument.body) walk(e.contentDocument); } catch (_) {}
      continue;
    }
    if (inter(e)) push(e);
  }
}
walk(document);
window.__bu = refs;
return JSON.stringify({
  url: location.href.slice(0, 300), title: document.title.slice(0, 120),
  sy: Math.round(scrollY), h: innerHeight,
  ph: Math.round(Math.max(document.documentElement.scrollHeight, document.body ? document.body.scrollHeight : 0)),
  lines: out, total: out.length
});
})()`

type snapResult struct {
	URL   string   `json:"url"`
	Title string   `json:"title"`
	Sy    int      `json:"sy"`
	H     int      `json:"h"`
	Ph    int      `json:"ph"`
	Lines []string `json:"lines"`
	Total int      `json:"total"`
}

func cmdSnap(c *cdpClient, args []string) error {
	max := 150
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
	raw, err := c.evalString(snapJS)
	if err != nil {
		return err
	}
	var r snapResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("bad snapshot: %v", err)
	}
	fmt.Printf("%s %q scroll=%d+%d/%d\n", r.URL, r.Title, r.Sy, r.H, r.Ph)
	lines := r.Lines
	if len(lines) > max {
		lines = lines[:max]
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	if r.Total > len(lines) {
		fmt.Printf("... %d more elements (use --max %d)\n", r.Total-len(lines), r.Total)
	}
	return nil
}

// resolveJS locates window.__bu[i-1], scrolls it into view, and returns its
// composed viewport center (walking up frame offsets for iframe elements).
const resolveJS = `((i) => {
const e = window.__bu && window.__bu[i - 1];
if (!window.__bu) return JSON.stringify({err: 'no snapshot in this page; run: use-browser snap'});
if (!e) return JSON.stringify({err: 'index [' + i + '] out of range (snapshot has ' + window.__bu.length + '); run: use-browser snap'});
if (!e.isConnected) return JSON.stringify({err: 'element [' + i + '] no longer in DOM; run: use-browser snap'});
e.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
const r = e.getBoundingClientRect();
let ox = 0, oy = 0, w = e.ownerDocument.defaultView;
while (w && w.frameElement) {
  const fr = w.frameElement.getBoundingClientRect();
  ox += fr.left; oy += fr.top;
  w = w.parent;
}
const x = ox + r.left + r.width / 2, y = oy + r.top + r.height / 2;
let cover = '';
const hit = document.elementFromPoint(x, y);
if (hit && hit !== e && !e.contains(hit) && !hit.contains(e) && hit.tagName !== 'IFRAME') {
  cover = '<' + hit.tagName.toLowerCase() + (hit.className && typeof hit.className === 'string' ? ' ' + hit.className.split(' ')[0] : '') + '>';
}
return JSON.stringify({x: Math.round(x), y: Math.round(y), tag: e.tagName.toLowerCase(), cover});
})(%d)`

type resolved struct {
	Err   string  `json:"err"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Tag   string  `json:"tag"`
	Cover string  `json:"cover"`
}

func (c *cdpClient) resolveIndex(i int) (*resolved, error) {
	raw, err := c.evalString(fmt.Sprintf(resolveJS, i))
	if err != nil {
		return nil, err
	}
	var r resolved
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("bad resolve result: %v", err)
	}
	if r.Err != "" {
		return nil, fmt.Errorf("%s", r.Err)
	}
	return &r, nil
}

func (c *cdpClient) mouseClick(x, y float64, button string, clicks int) error {
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		if _, err := c.Call("Input.dispatchMouseEvent", map[string]any{
			"type": typ, "x": x, "y": y, "button": button, "clickCount": clicks,
		}); err != nil {
			return err
		}
	}
	return nil
}

// cmdClick: `click 5` (snapshot index), `click 120,340` (coords),
// flags: --double, --right
func cmdClick(c *cdpClient, args []string) error {
	button, clicks := "left", 1
	var rest []string
	for _, a := range args {
		switch a {
		case "--double":
			clicks = 2
		case "--right":
			button = "right"
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: use-browser click <index | x,y> [--double] [--right]")
	}
	before, _ := c.evalString("location.href")
	var x, y float64
	note := ""
	if strings.Contains(rest[0], ",") {
		parts := strings.SplitN(rest[0], ",", 2)
		fx, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		fy, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("bad coordinates %q", rest[0])
		}
		x, y = fx, fy
	} else {
		i, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("usage: use-browser click <index | x,y>")
		}
		r, err := c.resolveIndex(i)
		if err != nil {
			return err
		}
		x, y = r.X, r.Y
		if r.Cover != "" {
			note = fmt.Sprintf(" (point is covered by %s — an overlay/dialog may be open)", r.Cover)
		}
	}
	if err := c.mouseClick(x, y, button, clicks); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	after, err := c.evalString("location.href")
	if err != nil || (after != before && after != "") {
		// navigation likely started; give it a moment
		c.waitReady(8 * time.Second)
		after, _ = c.evalString("location.href")
	}
	if after != "" && after != before {
		fmt.Printf("ok -> %s%s\n", after, note)
	} else {
		fmt.Printf("ok%s\n", note)
	}
	return nil
}

// fillJS focuses element i and selects its content so insertText replaces it.
const fillJS = `((i) => {
const e = window.__bu && window.__bu[i - 1];
if (!window.__bu) return JSON.stringify({err: 'no snapshot in this page; run: use-browser snap'});
if (!e) return JSON.stringify({err: 'index [' + i + '] out of range; run: use-browser snap'});
if (!e.isConnected) return JSON.stringify({err: 'element [' + i + '] no longer in DOM; run: use-browser snap'});
e.scrollIntoView({block: 'center', behavior: 'instant'});
e.focus();
if (typeof e.select === 'function') e.select();
else if (e.isContentEditable) {
  const sel = e.ownerDocument.getSelection(), rg = e.ownerDocument.createRange();
  rg.selectNodeContents(e); sel.removeAllRanges(); sel.addRange(rg);
}
return JSON.stringify({ok: true, tag: e.tagName.toLowerCase()});
})(%d)`

const fillNotifyJS = `((i) => {
const e = window.__bu[i - 1];
e.dispatchEvent(new Event('input', {bubbles: true}));
e.dispatchEvent(new Event('change', {bubbles: true}));
return (e.value !== undefined ? e.value : e.innerText || '').slice(0, 80);
})(%d)`

func cmdFill(c *cdpClient, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: use-browser fill <index> <text>")
	}
	i, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("usage: use-browser fill <index> <text>")
	}
	text := strings.Join(args[1:], " ")
	raw, err := c.evalString(fmt.Sprintf(fillJS, i))
	if err != nil {
		return err
	}
	var r struct {
		Err string `json:"err"`
		Tag string `json:"tag"`
	}
	json.Unmarshal([]byte(raw), &r)
	if r.Err != "" {
		return fmt.Errorf("%s", r.Err)
	}
	if _, err := c.Call("Input.insertText", map[string]any{"text": text}); err != nil {
		return err
	}
	got, err := c.evalString(fmt.Sprintf(fillNotifyJS, i))
	if err != nil {
		return err
	}
	fmt.Printf("ok <%s> = %q\n", r.Tag, got)
	return nil
}
