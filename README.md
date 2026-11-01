# use-browser

A browser CLI for coding agents. It is one Go binary with no dependencies, and it drives any Chromium browser (Chrome, Brave, Edge, Chromium, Vivaldi, Opera).

Agents read pages as short indexed text instead of screenshots, so a typical look at a page costs 200 to 600 tokens.

```
$ use-browser nav news.ycombinator.com
ok https://news.ycombinator.com/ "Hacker News"
$ use-browser snap
[3]<a> "new"
[12]<a> "OnePlus halts operations in USA and Europe"
[17]<a> "18 comments"
$ use-browser click 17
```

## Install

Windows:

```powershell
irm https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.ps1 | iex
```

macOS and Linux:

```bash
curl -fsSL https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.sh | sh
```

Or build it with Go 1.24+: `go build -o use-browser .`

To teach your agent the workflow, add the skill with `npx skills add hoangvu12/use-browser`, or save `use-browser skill` to your agent's skills folder.

## Connect a browser

Pick one of four ways. If you want your logins, use the extension.

**1. The extension (your real browser, logged in).** Run this once:

```
use-browser ext install brave
```

It registers use-browser with the browser and writes the extension folder. Then you load the extension once, as described in [Set up the extension](#set-up-the-extension). From then on, every command drives the browser you already have open, with your logins and tabs. There is no debug port, no profile copy, and no permission popup.

The agent never takes over your tabs. Its first `nav` opens its own background tab in a "use-browser" tab group. While it works, the browser shows its "started debugging this browser" bar. Click Cancel on that bar, or click the extension icon, to turn use-browser off.

**2. Clone your profile.** `use-browser clone brave` copies your profile to a separate folder and launches that copy with debugging on. You get your logins, but in a second window. On Windows the cookie file is locked while the browser runs, so `--close-source` closes your browser first to copy it.

**3. The inspect toggle.** `use-browser connect brave` uses the browser's own remote debugging switch. Chrome and Brave ask "Allow?" on every connection, so this only works with you at the keyboard.

**4. A clean profile.** `use-browser launch brave` starts a separate, empty profile. Use it when the task needs no logins.

If the extension is running, use-browser uses it. Otherwise it falls back to the other three. Run `use-browser doctor` to see which one you are on. With more than one browser installed, pin one with `use-browser use brave`.

## Set up the extension

If an agent sent you here, it already ran `use-browser ext install` for you. You only need to do this once per browser.

1. Type `brave://extensions` in the address bar. Use `chrome://extensions` in Chrome and `edge://extensions` in Edge.
2. Turn on **Developer mode** in the top right corner.
3. Click **Load unpacked** and choose the extension folder. The path is on your clipboard. On Windows it is `%LOCALAPPDATA%\use-browser\extension`.

That's it. A "use-browser bridge" icon appears in the toolbar. Tell your agent you're done, or run `use-browser ext status` yourself.

The icon also shows the extension's state. A number means use-browser is driving that many tabs. `OFF` means you turned it off, and clicking the icon turns it back on. A red `!` means the browser could not start use-browser; hover it for the reason and run `use-browser ext install` again.

Developer mode is needed because the extension is not on the Chrome Web Store. Browsers don't let programs install extensions into your profile, so this click can't be automated.

## Commands

```
use-browser nav <url>                navigate and wait for load
use-browser snap [--max N]           indexed interactive elements
use-browser text [--max N]           readable page text
use-browser click <i | x,y>          click an element or a point [--double --right]
use-browser fill <i> <text>          replace an input's value
use-browser type <text>              type into the focused element
use-browser key <k>                  Enter, Tab, esc, ctrl+a, shift+Tab
use-browser scroll [down|up|top|bottom|<px>]
use-browser shot [path] [--full]     screenshot, prints the file path
use-browser js <expr>                run JavaScript (js - reads stdin)
use-browser cdp <Domain.method> [json]   raw DevTools call

use-browser tabs                     list tabs with stable ids
use-browser tab <id>                 switch tab
use-browser open [url]               new tab, prints its id
use-browser close [id]               close a tab

use-browser ext install [browser]    set up the extension (uninstall to remove)
use-browser ext status [browser]     is it connected, and if not, what's missing
use-browser clone | connect | launch [browser]
use-browser stop [browser]           close a browser use-browser started
use-browser use [browser]            pin a browser (auto clears it)
use-browser doctor                   show what is connected and how
```

Run several commands over one connection by piping them in:

```bash
use-browser <<'EOF'
fill 3 "user@example.com"
click 5
snap
EOF
```

Snapshot indexes live in the page, so `click 17` works in a later command. If the page changed, the command fails and tells you to snap again.

## Tabs and parallel agents

Address tabs by the 8-character id from `use-browser tabs`, not by position, because positions change. `--tab <id>` runs one command against a tab without switching to it.

Give each agent its own session with `BU_SESSION=<name>`. A session keeps its own current tab and pinned browser, so two agents don't type into each other's pages.

## Optional: Jev

With a [TypeSafe](https://console.typesafe.ai) key (`use-browser apikey set <key>`), two more commands work. `find "accept cookies" --click` picks an element without pulling a snapshot into context. `run "<goal>"` completes a short click-and-type task in one call and stops at login or payment pages. Nothing calls TypeSafe unless you run one of these.

## How it works

Each command opens a connection, acts, and exits. There is no daemon. The only saved state is the current tab id, in a small JSON file.

Without the extension, use-browser speaks the DevTools protocol over a WebSocket to the browser's debug port. With the extension, the browser starts use-browser as a native messaging host. use-browser then listens on a local port protected by a token. The extension passes each DevTools command to `chrome.debugger`, which needs no debug port. Claude in Chrome works the same way.

Output is one `ok` line or compact data, and errors go to stderr as `error: ...`. Everything a CLI prints ends up in the agent's context, so it prints as little as possible.

On Windows PowerShell, pass anything with quotes through stdin (`js -` or batch mode), because PowerShell 5.1 mangles inline quotes.
