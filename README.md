# use-browser

A small browser CLI for coding agents. It's a single Go binary with no dependencies that drives a real browser over the DevTools protocol. Any Chromium-family browser works: Chrome, Brave, Edge, Chromium, Vivaldi, Opera.

The idea came from using [browser-use](https://github.com/browser-use/browser-use): the "coding agent drives the browser" model is right, but installing it pulls in about 40 Python packages, including five LLM SDKs the CLI never touches, and every look at a page tends to go through a screenshot. This tool keeps the model and drops the weight.

|  | browser-use CLI | use-browser |
|---|---|---|
| Install | `pip install browser-use`, ~40 packages | one 6 MB binary |
| Runtime | Python 3.11+ plus a background daemon | none |
| Round trip per command | Python startup plus daemon IPC | measured 53 ms |
| Telemetry | posthog, on by default | none |
| How the agent reads a page | screenshot, roughly 1,500 tokens each | indexed text snapshot, usually 200 to 600 tokens |

## Install

Windows:

```powershell
irm https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.ps1 | iex
```

macOS and Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.sh | sh
```

If you'd rather build from source, `go build -o use-browser .` with Go 1.24 or newer is the whole process.

## Connect a browser

`use-browser doctor` lists the browsers it found and how to connect. There are three ways, because Chromium 136 [stopped honoring](https://developer.chrome.com/blog/remote-debugging-port) `--remote-debugging-port` on a browser's default profile:

Pin the browser before connecting whenever more than one Chromium-family
browser is installed or running:

```
use-browser use chrome
use-browser launch chrome
use-browser doctor chrome
```

The pin scopes endpoint and profile discovery to Chrome, so an existing Brave
debugging endpoint cannot be selected accidentally. `doctor` should mark Chrome
`<- pinned`; `use-browser use auto` clears the pin. If a source checkout's
rebuilt binary supports `use` but the PATH-installed binary does not, use the
rebuilt binary or update the installation before connecting.

**Your real profile, no toggle** — clone it and drive the copy:

```
use-browser clone brave
```

This copies your real profile (cookies, logins) into a non-default directory next to the CLI's state, then launches the browser against the copy with `--remote-debugging-port` baked in. Because the copy lives at a non-default path, Chromium 136 allows flag-based debugging there — so you get your logins with no `chrome://inspect` toggle and no popup. This is the same fallback [browser-use uses](https://github.com/browser-use/browser-use/issues/1520). Pick a specific profile with `--profile "Profile 1"`, and refresh the copy from your real profile with `--fresh`. Close the source browser first for a complete copy (a running browser can hold cookie files locked). Your day-to-day browser is only read, never launched with debugging.

**Your real profile, with a toggle** — attach to the browser you're already running:

```
use-browser connect
```

This opens `chrome://inspect/#remote-debugging` (`brave://inspect/#remote-debugging` in Brave, and so on) in whichever browser you're running, and waits while you enable "Allow remote debugging for this browser instance". One toggle, once. Nothing gets around the restriction here, since the whole point is that a human has to approve it in the live profile.

**A clean, separate profile** — keep automation isolated:

```
use-browser launch brave
```

This starts the browser you name (or the first one detected) with a dedicated empty profile stored next to the CLI's state. Logins you make there stick around for future runs, and your day-to-day browser never sees any of it.

`BU_CDP_URL` points the CLI at any other DevTools endpoint, including a remote or cloud browser.

## How it works

`use-browser snap` injects one JavaScript pass that walks the DOM, including open shadow roots and same-origin iframes, and prints only the visible interactive elements:

```
$ use-browser snap
https://news.ycombinator.com/ "Hacker News" scroll=0+485/1249
[3]<a> "new"
[12]<a> "OnePlus halts operations in USA and Europe"
[17]<a> "18 comments"
...
```

The agent acts by index: `use-browser click 17`, `use-browser fill 4 "query"`. Element references live in the page itself (a `window.__bu` array), so an index from a previous invocation still resolves to the live element. When the page has navigated or the element is gone, the command fails with a message telling the agent to snap again. Screenshots exist (`use-browser shot`) but are the fallback for canvas apps and visual questions, not the default way to see a page.

There is deliberately no daemon. browser-use runs one to keep its CDP connection and session state alive between invocations. In Go, opening a fresh WebSocket to Chrome costs around 10 ms, so each command just connects, acts, and exits. The only persistent state is the current tab id in a small JSON file.

## Choosing a browser

With one Chromium installed there is nothing to do. With several, pin one — the pin persists, so it is said once:

```bash
use-browser use chrome        # persists
use-browser --browser chrome snap   # one command only, writes nothing
```

With nothing pinned and two browsers debuggable, discovery stops rather than guessing:

```
$ use-browser snap
error: chrome and brave both accept a debug connection, and no browser is pinned.
Pin one — it persists, so you only say it once:
  use-browser use chrome
Or override a single command: use-browser --browser chrome <command>
```

Guessing here is how an agent ends up driving the Brave you asked it to leave alone. The ambiguous case is the only one that stops; a single browser still needs no configuration.

`use-browser profiles [browser]` lists your real profiles with the names your browser shows you, so `clone --profile` is discoverable:

```
$ use-browser profiles chrome
chrome profiles (~/AppData/Local/Google/Chrome/User Data):
  Default      "Personal"
  Profile 1    "Work"
copy one and drive it: use-browser clone chrome --profile "Default"
```

### Driving your real profile, and the permission dialog

There are two routes to your everyday profile, and only one of them works unattended.

**Flags don't work.** Chromium 136 ignores `--remote-debugging-port` whenever `--user-data-dir` is the browser's default one — it starts, and serves no endpoint. That is a deliberate fix for the "point a debug port at someone's live cookies" attack, and passing the same path explicitly doesn't dodge it. This restriction is the entire reason `clone` exists.

**The `chrome://inspect` toggle works, at a price.** That is `use-browser connect`, and it drives the real profile with no copy at all. But on Chrome and Brave 144+ the "Allow remote debugging?" dialog is **per connection, not per session** — measured here, not assumed:

```
$ time use-browser js "location.host"     # after a previous Allow click
error: browser websocket ws://127.0.0.1:63915/...: i/o timeout
real    1m0.031s
```

One accepted click buys exactly one connection. use-browser opens one per invocation, so every invocation needs a human. A daemon would only move the click to once-per-daemon-lifetime, so it doesn't rescue unattended use either. Batch mode is the real lever: one invocation is one connection, so a whole task costs one click.

So for anything unattended, `clone` is the mechanism — flag-based debugging on a copied profile never prompts. `use-browser doctor` tells you which mode you are in.

### Keeping a clone current

A copy drifts, so `clone` refreshes an existing clone before launching it, moving only changed files:

```
$ use-browser clone brave
synced 6176 file(s), 884.0 MB, 45 skipped     # first pass
$ use-browser clone brave
synced 42 file(s), 32.9 MB, 43 skipped        # 1.2s
```

Modification times are preserved so the second pass has something to compare against. SQLite databases move as a group with their `-wal`/`-shm`/`-journal` sidecars, and sidecars the source has dropped are deleted from the copy — a fresh `Cookies` beside a stale journal reads as corrupt. Close the real browser first if you want a complete sync; files it holds open are skipped, and cookies are usually among them. `--no-sync` attaches unchanged, `--fresh` re-copies from scratch.

If the clone itself is running, `clone` attaches to it rather than writing into a live profile — which is also how a second session joins the first session's cloned browser. That check uses Chromium's singleton lock (`lockfile` on Windows, `SingletonLock` on POSIX), because Chrome and Brave frequently never write `DevToolsActivePort`.

### Disk

```
$ use-browser clean
  profile-brave-clone      677.9 MB
  profile-chrome           892.6 MB
  total                   2080.9 MB
```

`use-browser clean <name>...` or `--all` deletes them, skipping any that a browser is using. A deleted clone is re-copied on the next `clone`. `BU_HOME=<dir>` relocates profiles and state; the default is the OS cache directory.

## Tabs and parallel agents

`use-browser tabs` gives every tab a stable 8-character id, and that is how you should address them:

```
$ use-browser tabs
1  0a3328d8 "Example Domain" https://example.com/
2* 5f44a085 "Hacker News" https://news.ycombinator.com/
```

The leading positions are for humans. They come from `Target.getTargets`, which is neither tab-strip order nor stable — open a tab and an existing one can slide from `2` to `4` — so an agent that remembers a position ends up driving a different page. The id doesn't move, and `use-browser open` prints the id of the tab it just made.

`--tab <id>` runs a single command against a tab without switching to it, reading and writing no state at all:

```bash
a=$(use-browser open https://example.com | awk '{print $2}')
b=$(use-browser open https://example.org | awk '{print $2}')
use-browser --tab "$a" snap
use-browser --tab "$b" click 3
```

Snapshot indexes live in the page (`window.__bu`), so they're already per-tab: `[5]` in one tab has nothing to do with `[5]` in another.

For several agents at once, `BU_SESSION=<name>` (or `--session <name>`) gives each its own state file, so "current tab" stops being a machine-wide singleton:

```bash
BU_SESSION=research use-browser open https://example.com
BU_SESSION=checkout use-browser open https://shop.example.com
```

A session also scopes the pinned browser, the debug port and the launch profile, so `BU_SESSION=a use-browser launch chrome` and `BU_SESSION=b use-browser launch chrome` really are two browsers. Clones are the deliberate exception: they are shared by every session, one copy per browser rather than one per agent, because a clone runs to hundreds of megabytes. Two sessions cloning the same browser get one browser and a tab each. A named session never adopts a browser it didn't start — otherwise the port probe would hand session `b` the instance session `a` launched — so each one launches, clones, connects, or gets pointed at an endpoint with `BU_CDP_URL`. Agents that should share the user's logins can all point at the same `BU_CDP_URL` and keep to their own tab ids.

When the remembered tab is gone, page commands fail with `current tab <id> is gone` instead of quietly adopting whatever tab happens to be first — that silent adoption is how one agent ends up typing into another's page. `tabs` and `tab` keep working, since they're how you recover.

## Commands

```
use-browser nav <url>                     navigate and wait for load
use-browser snap [--max N]                indexed interactive elements
use-browser text [--max N]                readable page text, capped at 4000 chars by default
use-browser click <i | x,y>               click an element index or coordinates [--double --right]
use-browser fill <i> <text>               focus element i and replace its value
use-browser type <text>                   type into the focused element
use-browser key <k>                       Enter, Tab, esc, down, ctrl+a, shift+Tab
use-browser scroll [down|up|top|bottom|<px>]
use-browser shot [path] [--full]          screenshot PNG, prints the file path
use-browser tabs                          list tabs: "2* a1b2c3d4 "title" url"
use-browser tab <id | n>                  switch current tab
use-browser open [url]                    new tab, prints its id
use-browser close [id]                    close the current tab, or the one named
use-browser js <expr>                     run JavaScript in the page (js - reads stdin)
use-browser cdp <Domain.method> [json]    raw DevTools call for anything not covered above
use-browser use [browser]                 pin browser selection (`auto` clears it)
use-browser profiles [browser]            list that browser's real profiles, for clone
use-browser clean [name|--all]            list or delete use-browser's own profiles
use-browser doctor [browser] | skill | help

--tab <id>                                run one command against a tab, writing no state
--session NAME                            scope this agent's state (env: BU_SESSION)
--browser NAME                            one-shot browser override (env: BU_BROWSER)
```

Multi-step flows go through batch mode, which runs command lines from stdin over a single connection:

```bash
use-browser <<'EOF'
fill 3 "user@example.com"
fill 4 "hunter2"
click 5
snap
EOF
```

Execution stops at the first error with the failing line number and exit code 1.

## Using it with an agent

The skill that teaches an agent the whole workflow lives in [SKILL.md](SKILL.md) and installs into Claude Code, Cursor, Codex, and about 70 other agents through the [skills.sh](https://skills.sh) ecosystem:

```bash
npx skills add hoangvu12/use-browser
```

The skill includes the binary install steps, so an agent that has the skill but not the binary can set itself up. Without npx, `use-browser skill` prints the same file and you can put it wherever your agent reads skills from:

```bash
mkdir -p .claude/skills/use-browser
use-browser skill > .claude/skills/use-browser/SKILL.md
```

## Design notes

The output contract is the part I'd defend hardest: every command prints a single `ok` line or compact data, and errors go to stderr as `error: ...`. There are no banners and no spinners, because every byte a CLI prints ends up in an agent's context window and gets paid for on every request after that.

CDP itself turned out to be the easy part. It's JSON-RPC over one WebSocket, and the client in `ws.go` is about 180 lines of standard library code. The hard part was deciding what the snapshot should and shouldn't include; the current walker skips invisible elements, dedupes wrappers like a span inside a button, and flags when a click target is covered by an overlay, since a silent click on a cookie banner wastes an entire agent round trip.

One Windows note: PowerShell 5.1 mangles quotes in inline arguments and prefixes piped input with a BOM. The CLI strips the BOM, and anything with quotes in it should go through stdin (`js -` or batch mode) rather than inline arguments.
