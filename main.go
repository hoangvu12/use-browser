package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const version = "0.5.1"

const help = `use-browser ` + version + ` — tiny browser CLI for coding agents (CDP, zero deps)

Page state:
  use-browser snap [--max N]        indexed interactive elements  ->  [5]<button> "Sign in"
  use-browser text [--max N]        readable page text (truncated)
  use-browser shot [path] [--full]  screenshot PNG -> file path

Actions:
  use-browser nav <url>             navigate current tab, wait for load
  use-browser click <i | x,y>       click snapshot index or coordinates [--double --right]
  use-browser fill <i> <text>       focus element i, replace its value
  use-browser type <text>           type into the focused element
  use-browser key <key>             Enter, Tab, esc, down, ctrl+a, shift+Tab ...
  use-browser scroll [down|up|top|bottom|<px>]

Tabs (the ids are stable, the 1..N indexes are not):
  use-browser tabs                  list tabs -> 2* a1b2c3d4 "title" url
  use-browser tab <id | n>          switch current tab
  use-browser open [url]            new tab, prints its id
  use-browser close [id]            close current tab (or the one named)

Several tabs, several agents:
  --tab <id>      run one command against a tab without switching to it.
                  Stateless: reads and writes no current-tab state.
                  Goes before the command: use-browser --tab a1b2c3d4 snap
  --session NAME  give this agent its own current tab, browser pin and port
                  (env: BU_SESSION=NAME). Sessions cannot see each other's
                  state, so parallel agents stop fighting over "current tab".

Escape hatches:
  use-browser js <expr>             run JS in the page (or pipe on stdin: use-browser js -)
  use-browser cdp <Domain.method> [params-json]

Batch (one invocation, one connection):
  use-browser <<'EOF'
  nav https://example.com
  snap
  EOF

Setup:
  use-browser use [browser]       pin which browser to drive (auto = unpin)
  use-browser profiles [browser]  list that browser's real profiles, for clone
  use-browser clean [name|--all]  list or delete use-browser's own profiles
                                  --cache drops only caches, keeping logins
  use-browser connect [browser]   attach to your normal browser, real profile (one-time toggle)
  use-browser clone [browser]     copy your real profile & launch it debuggable (logins, no toggle)
  use-browser launch [browser]    start a browser with a dedicated automation profile
  use-browser doctor [browser]    list installed browsers, diagnose the connection

clone options: --profile "Profile 1"  pick which profile to copy | --fresh  re-copy
               --no-sync  attach without refreshing | --port N
               --close-source  close your real browser first, then reopen its
               tabs in the clone. The only way to sync logins with no clicks:
               a running browser holds its cookie file exclusively.
An existing clone is refreshed from your real profile on every clone command,
so logins you have made since last time come across. Only changed files move,
and files your real profile has deleted are dropped from the copy.

Profiles and state live in the OS cache dir; BU_HOME=<dir> moves them.

Works with any Chromium-family browser (Chrome, Brave, Edge, Chromium, Vivaldi, Opera).
Without a pin, whichever Chromium is already running wins. Pin one to keep the
rest out of it — e.g. drive Chrome while Brave stays open and untouched:
  use-browser use chrome && use-browser clone chrome
Per-command override: --browser chrome (or BU_BROWSER=chrome), which writes no
state. With nothing pinned and two browsers debuggable, use-browser stops and
asks rather than guessing. Remote endpoint: BU_CDP_URL=http://host:port.

Other: use-browser skill | use-browser version`

// Global flags, accepted before or after the subcommand.
var (
	flagSession string // --session NAME / BU_SESSION: state scope for this agent
	flagTab     string // --tab <id>: one-shot tab override, writes no state
	flagBrowser string // --browser NAME: one-shot browser override, writes no state
)

// validSessionName guards the name before it becomes part of a file path.
func validSessionName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

// parseGlobalFlags consumes --session/--tab from the front of the argument
// list and returns the rest. Leading-only on purpose: scanning the whole list
// would eat a literal "--tab" out of `use-browser fill 3 "--tab x"`.
func parseGlobalFlags(args []string) ([]string, error) {
	flagSession = strings.TrimSpace(os.Getenv("BU_SESSION"))
	i := 0
	for i < len(args) {
		name := args[i]
		if name != "--session" && name != "--tab" && name != "--browser" {
			break
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s needs a value", name)
		}
		switch name {
		case "--session":
			flagSession = strings.TrimSpace(args[i+1])
		case "--tab":
			flagTab = strings.TrimSpace(args[i+1])
		case "--browser":
			flagBrowser = strings.TrimSpace(args[i+1])
		}
		i += 2
	}
	if flagSession != "" && !validSessionName(flagSession) {
		return nil, fmt.Errorf("invalid session name %q: use 1-64 characters from [A-Za-z0-9_-]", flagSession)
	}
	return args[i:], nil
}

// commands that need a page connection
var pageCommands = map[string]func(*cdpClient, []string) error{
	"nav": cmdNav, "snap": cmdSnap, "click": cmdClick, "fill": cmdFill,
	"type": cmdType, "key": cmdKey, "scroll": cmdScroll, "text": cmdText,
	"js": cmdJS, "shot": cmdShot, "cdp": cmdCDP,
	"tabs": cmdTabs, "tab": cmdTab, "open": cmdOpen, "close": cmdClose,
}

func main() {
	args, err := parseGlobalFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	if len(args) == 0 {
		// batch mode from stdin, or help on a TTY
		fi, _ := os.Stdin.Stat()
		if fi != nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			fmt.Println(help)
			return
		}
		os.Exit(runBatch())
	}
	switch args[0] {
	case "help", "--help", "-h":
		fmt.Println(help)
		return
	case "version", "--version", "-v":
		fmt.Println("use-browser " + version)
		return
	case "skill":
		fmt.Println(skillText)
		return
	case "doctor":
		if err := cmdDoctor(args[1:]); err != nil {
			os.Exit(1)
		}
		return
	case "use":
		if err := cmdUse(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "launch":
		if err := cmdLaunch(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "clone":
		if err := cmdClone(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "clean":
		if err := cmdClean(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "profiles":
		if err := cmdProfiles(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	case "connect":
		if err := cmdConnect(args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fn, ok := pageCommands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "error: unknown command %q (see: use-browser help)\n", args[0])
		os.Exit(2)
	}
	c, err := connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer c.Close()
	if err := fn(c, args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runBatch executes command lines from stdin over a single connection.
// Blank lines and #-comments are skipped; stops at the first error.
func runBatch() int {
	c, err := connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	defer c.Close()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if lineNo == 1 {
			line = strings.TrimPrefix(line, "\ufeff") // PowerShell pipes a BOM
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens, err := splitLine(line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error (line %d): %v\n", lineNo, err)
			return 1
		}
		if len(tokens) == 0 {
			continue
		}
		fn, ok := pageCommands[tokens[0]]
		if !ok {
			fmt.Fprintf(os.Stderr, "error (line %d): unknown command %q\n", lineNo, tokens[0])
			return 1
		}
		if err := fn(c, tokens[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error (line %d, %s): %v\n", lineNo, tokens[0], err)
			return 1
		}
		// `tab` and `open` re-attach on the live connection, so only a closed
		// tab needs a fresh one. A --tab override dies with the tab it named.
		if tokens[0] == "close" {
			flagTab = ""
			c.Close()
			c, err = connect()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error (line %d): reconnect: %v\n", lineNo, err)
				return 1
			}
		}
	}
	return 0
}

// splitLine tokenizes a batch line with double/single-quote support.
func splitLine(s string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inTok := false
	var quote byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else if ch == '\\' && quote == '"' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(ch)
			}
		case ch == '"' || ch == '\'':
			quote = ch
			inTok = true
		case ch == ' ' || ch == '\t':
			if inTok {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteByte(ch)
			inTok = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unclosed quote")
	}
	if inTok {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}
