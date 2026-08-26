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
use-browser tabs / tab <n> / open [url] / close
use-browser js <expr>                     run JavaScript in the page (js - reads stdin)
use-browser cdp <Domain.method> [json]    raw DevTools call for anything not covered above
use-browser use [browser]                 pin browser selection (`auto` clears it)
use-browser doctor [browser] | skill | help
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
