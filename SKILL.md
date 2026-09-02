---
name: use-browser
description: Controls a real Chrome browser from the command line for web automation, scraping, form filling, testing, and screenshots. Use when the user asks to browse, open, click, read, or extract data from a website, or to automate any task in the browser. Works through indexed text snapshots instead of screenshots, so most page interactions cost a few hundred tokens.
---

# use-browser

Single-binary CLI that drives any Chromium-family browser (Chrome, Brave, Edge, Chromium, Vivaldi, Opera) over the DevTools protocol. It needs a browser serving remote debugging on 127.0.0.1:9222; `BU_CDP_URL` points at any other DevTools endpoint.

## Pin the browser first

If more than one Chromium-family browser is installed, pin one before anything
else. The pin is written to disk and persists, so the user says which browser
they want **once**, not every session:

```bash
use-browser use chrome
use-browser launch chrome
use-browser doctor chrome
```

With nothing pinned and two browsers debuggable, `use-browser` refuses to guess
and tells you to pin. Do not work around that by picking one — pin the browser
the user named, and if they have not named one, ask.

To override for a single command without changing the pin, put `--browser`
before the command: `use-browser --browser chrome snap`. `BU_BROWSER=chrome`
does the same thing. Neither writes state.

Completion criterion: `doctor` marks the requested browser `<- pinned`, and its
endpoint belongs to that browser. If `use-browser use` is unknown but this skill
comes from a local source checkout containing a sibling `use-browser` binary,
prefer that rebuilt binary over an older PATH installation; otherwise rebuild
or update the CLI before continuing. Never fall back to attaching to a different
already-running browser.

If a command fails to connect, run `use-browser doctor`. It lists installed browsers and prints the three ways to connect:

- `use-browser clone [browser]` copies the user's real profile (cookies, logins) into a non-default directory and launches the browser against the copy with flag-based remote debugging. No toggle, no popup, but real logins — the Chromium 136 restriction only applies to the default profile path. `--profile "Profile 1"` picks a profile; `--fresh` re-copies. Tell the user to close the source browser first for a complete copy. The real profile is only read, never launched with debugging.
- `use-browser connect [browser]` attaches to the user's already-running browser with their real profile. **Use this only when the user is at the keyboard, and prefer `clone` otherwise.** On Chrome and Brave 144+ the "Allow remote debugging?" dialog is **per connection, not per session**: an accepted click buys exactly one connection, and the next invocation prompts again. `use-browser` opens one connection per invocation, so unattended work is impossible in this mode. Batch mode is the only lever - one invocation is one connection, so a whole task costs one click. The user must enable the toggle themselves; never flip security toggles on their behalf.
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

## Keeping a clone current

A clone is a copy, so it drifts from the real profile. Every `use-browser clone
<browser>` refreshes an existing clone before launching it, copying only the
files that changed:

```
$ use-browser clone brave
syncing ...\\Brave-Browser\\User Data\\Default -> ...\\profile-brave-clone\\Default ...
synced 42 file(s), 32.9 MB, 43 skipped (locked or unreadable)
```

- Tell the user to close the real browser first. Files it holds open are
  skipped, and cookies are usually among them.
- `--no-sync` attaches to the existing clone unchanged. `--fresh` re-copies
  everything from scratch.
- If the clone itself is already running, `use-browser` attaches to it instead
  of syncing. Writing into a live profile corrupts its databases. This is also
  what happens when a second session clones the same browser: it joins the
  running one rather than making a second copy.

## Disk

Clones are large. `use-browser clean` lists what has accumulated and deletes
what you name:

```
$ use-browser clean
  profile-brave-clone      677.9 MB
  profile-chrome           892.6 MB
  total                   2080.9 MB
```

`use-browser clean <name>...` or `--all` removes them; a deleted clone is
re-copied on the next `clone`. `BU_HOME=<dir>` moves profiles and state
somewhere else entirely.

## Browser profiles

`use-browser profiles [browser]` lists the user's real profiles and the names
they see in the browser's own profile menu:

```
$ use-browser profiles chrome
chrome profiles (C:\\...\\User Data):
  Default      "Personal"
  Profile 1    "Work"
copy one and drive it: use-browser clone chrome --profile "Default"
```

Pass the directory name (`Default`, `Profile 1`) to `clone --profile`, not the
display name. Ask the user which profile to use when there is more than one.

## Tabs

`use-browser tabs` prints one line per tab: a position, a `*` on the current
one, then a **stable 8-character tab id**, the title and the URL.

```
1  0a3328d8 "Example Domain" https://example.com/
2* 5f44a085 "Docs" https://docs.example.com/
```

Address tabs by **id**, never by position. CDP does not report tabs in
tab-strip order and the order changes whenever any tab opens or closes — a
tab that was `2` a moment ago can be `4` now, in someone else's page. The id
never moves. `use-browser open` prints the id of the tab it created; record it.

```bash
id=$(use-browser open https://example.com | awk '{print $2}')
use-browser --tab "$id" snap
use-browser --tab "$id" click 5
use-browser close "$id"
```

`--tab <id>` runs one command against that tab **without switching to it** and
without reading or writing any current-tab state. Prefer it whenever you are
juggling more than one tab: `tab <id>` + a second command is two round trips
and anything can move the current tab in between.

Snapshot indexes are per-tab (they live in the page), so `[5]` in one tab and
`[5]` in another are unrelated. They still go stale on navigation — re-`snap`
the tab you are about to act on.

If a command reports `current tab <id> is gone`, the tab you were on was
closed. Run `use-browser tabs` and pick another with `use-browser tab <id>`.

## Several agents, one machine

Without a session, "current tab" is one machine-wide slot: a second agent
running `use-browser tab ...` silently repoints the first agent's `nav`. Give
each agent its own session:

```bash
export BU_SESSION=research      # per agent; [A-Za-z0-9_-], max 64 chars
use-browser open https://example.com
```

A session owns its own current tab, browser pin, debug port and launch
profile. Sessions cannot see each other's state.

Two ways to run in parallel:

- **Shared cloned browser, one tab each** — the normal way to run several
  agents against the user's real logins. A clone is shared by every session:
  one copy per browser, never one per agent, because it runs to hundreds of
  megabytes. The first session to run `use-browser clone <browser>` launches
  it; later sessions attach to the same browser and keep to their own tab.
- **Own empty browser per agent** — `BU_SESSION=a use-browser launch chrome`
  gives session `a` its own instance, profile and port. `launch` profiles are
  per session, so this is fully isolated, but it starts with no logins. Use it
  when tasks must not share cookies. A named session never adopts a browser it
  did not start, so each agent launches its own or is pointed at one with
  `BU_CDP_URL`.

Keep one working tab per task. Before opening another, check `use-browser tabs`
and reuse a matching one. Never close a tab you did not open.

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
use-browser tabs                   list tabs: "2* a1b2c3d4 \"title\" url"
use-browser tab <id | n>           switch current tab
use-browser open [url]             new tab, prints its id
use-browser close [id]             close current tab, or the one named
use-browser js <expr>              run JavaScript in the page
use-browser cdp <Domain.method> [params-json]   raw DevTools call
use-browser use [browser]          pin browser selection (`auto` clears it)
use-browser profiles [browser]     list that browser's real profiles, for clone
use-browser clean [name|--all]     list or delete use-browser's own profiles
use-browser connect [browser]      attach to the user's running browser (real profile, toggle)
use-browser clone [browser]        copy real profile & launch it debuggable (logins, no toggle)
use-browser launch [browser]       start a browser with a dedicated automation profile
use-browser doctor [browser]       list browsers and verify the requested pin

--tab <id>                         run one command against a tab, no state written
--session NAME                     scope this agent's state (env: BU_SESSION)
--browser NAME                     one-shot browser override (env: BU_BROWSER)
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
- Tab positions from `tabs` are unstable; the 8-character id is not. Hold ids, never positions.
- Never pick a browser for the user when `use-browser` says two are debuggable. Pin the one they named, or ask.
