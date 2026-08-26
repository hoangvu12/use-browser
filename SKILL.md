---
name: use-browser
description: Controls a real Chrome browser from the command line for web automation, scraping, form filling, testing, and screenshots. Use when the user asks to browse, open, click, read, or extract data from a website, or to automate any task in the browser. Works through indexed text snapshots instead of screenshots, so most page interactions cost a few hundred tokens.
---

# use-browser

Single-binary CLI that drives any Chromium-family browser (Chrome, Brave, Edge, Chromium, Vivaldi, Opera) over the DevTools protocol. It needs a browser serving remote debugging on 127.0.0.1:9222; `BU_CDP_URL` points at any other DevTools endpoint.

## Pin the browser first

When the user names a browser, run `use-browser use <browser>` before `launch`,
`clone`, `connect`, or `doctor`. Pinning keeps another open Chromium browser
from winning endpoint discovery. For an isolated Chrome session, the safe
sequence is:

```bash
use-browser use chrome
use-browser launch chrome
use-browser doctor chrome
```

Completion criterion: `doctor` marks the requested browser `<- pinned`, and its
endpoint belongs to that browser. If `use-browser use` is unknown but this skill
comes from a local source checkout containing a sibling `use-browser` binary,
prefer that rebuilt binary over an older PATH installation; otherwise rebuild
or update the CLI before continuing. Never fall back to attaching to a different
already-running browser.

If a command fails to connect, run `use-browser doctor`. It lists installed browsers and prints the three ways to connect:

- `use-browser clone [browser]` copies the user's real profile (cookies, logins) into a non-default directory and launches the browser against the copy with flag-based remote debugging. No toggle, no popup, but real logins — the Chromium 136 restriction only applies to the default profile path. `--profile "Profile 1"` picks a profile; `--fresh` re-copies. Tell the user to close the source browser first for a complete copy. The real profile is only read, never launched with debugging.
- `use-browser connect [browser]` attaches to the user's already-running browser with their real profile. It opens the browser's inspect page and waits; the user must enable "Allow remote debugging for this browser instance" themselves. Tell them to click it, and never flip security toggles on their behalf.
- `use-browser launch [chrome|brave|edge|...]` starts a detected browser with a dedicated empty automation profile, isolated from the user's own browsing. Logins made in it persist between runs.

If `use-browser` is not on PATH, install it first:

- Windows: `irm https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.ps1 | iex`
- macOS/Linux: `curl -fsSL https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.sh | sh`

## Workflow

1. `use-browser nav <url>` prints `ok <url> "title"` when the page has loaded.
2. `use-browser snap` prints the page's interactive elements with indexes:
   ```
   [5]<a> "Sign in"
   [7]<input:email> "Email"
   ```
3. Act on an element by its index: `use-browser click 5`, `use-browser fill 7 "user@example.com"`, `use-browser key Enter`.
4. After the page changes, run `snap` again. Indexes belong to one snapshot; acting on a stale index returns an error that says `run: use-browser snap`. Never guess indexes.
5. Read content with `use-browser text`, or extract structured data with `use-browser js`.

Take a screenshot only when text output cannot answer the question (canvas apps, maps, visual layout): `use-browser shot` prints a PNG path to view.

## Batch mode

Group multi-step actions into one invocation. Pipe commands on stdin, one per line:

```bash
use-browser <<'EOF'
fill 3 "user@example.com"
fill 4 "hunter2"
click 5
snap
EOF
```

Execution stops at the first error and prints the failing line number. Snap first in a separate invocation, then batch the actions: you need to see the indexes before you can use them.

## Commands

```
use-browser nav <url>              navigate current tab, wait for load
use-browser snap [--max N]         indexed interactive elements
use-browser text [--max N]         readable page text (default cap 4000 chars)
use-browser click <i | x,y>        click element index or coordinates [--double --right]
use-browser fill <i> <text>        focus element i and replace its value
use-browser type <text>            type into the focused element
use-browser key <k>                Enter, Tab, esc, down, ctrl+a, shift+Tab
use-browser scroll [down|up|top|bottom|<px>]
use-browser shot [path] [--full]   screenshot PNG, prints file path
use-browser tabs                   list tabs (* marks current)
use-browser tab <n>                switch current tab
use-browser open [url]             new tab
use-browser close                  close current tab
use-browser js <expr>              run JavaScript in the page
use-browser cdp <Domain.method> [params-json]   raw DevTools call
use-browser use [browser]          pin browser selection (`auto` clears it)
use-browser connect [browser]      attach to the user's running browser (real profile, toggle)
use-browser clone [browser]        copy real profile & launch it debuggable (logins, no toggle)
use-browser launch [browser]       start a browser with a dedicated automation profile
use-browser doctor [browser]       list browsers and verify the requested pin
```

## Extracting data

Prefer `js` with a precise expression over `text` when you need structured data:

```bash
use-browser js - <<'EOF'
JSON.stringify([...document.querySelectorAll("h2 a")].slice(0, 20).map(a => a.textContent.trim()))
EOF
```

Pass JavaScript on stdin with `js -` whenever the expression contains quotes; shells mangle inline quoting, stdin does not.

## Rules

- `click` warns when the target point is covered, for example `(point is covered by <div cookie-banner>)`. Handle the overlay before retrying.
- Stop and ask the user at login walls. Never enter credentials the user did not provide in this session.
- Errors go to stderr as `error: ...` with exit code 1.
