package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const version = "0.1.0"

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

Tabs:
  use-browser tabs                  list tabs (* = current)
  use-browser tab <n>               switch current tab
  use-browser open [url]            new tab
  use-browser close                 close current tab

Escape hatches:
  use-browser js <expr>             run JS in the page (or pipe on stdin: use-browser js -)
  use-browser cdp <Domain.method> [params-json]

Batch (one invocation, one connection):
  use-browser <<'EOF'
  nav https://example.com
  snap
  EOF

Setup:
  use-browser connect [browser]   attach to your normal browser, real profile (one-time toggle)
  use-browser launch [browser]    start a browser with a dedicated automation profile
  use-browser doctor              list installed browsers, diagnose the connection

Works with any Chromium-family browser (Chrome, Brave, Edge, Chromium, Vivaldi, Opera).
Remote endpoint: set BU_CDP_URL=http://host:port.

Other: use-browser skill | use-browser version`

// commands that need a page connection
var pageCommands = map[string]func(*cdpClient, []string) error{
	"nav": cmdNav, "snap": cmdSnap, "click": cmdClick, "fill": cmdFill,
	"type": cmdType, "key": cmdKey, "scroll": cmdScroll, "text": cmdText,
	"js": cmdJS, "shot": cmdShot, "cdp": cmdCDP,
	"tabs": cmdTabs, "tab": cmdTab, "open": cmdOpen, "close": cmdClose,
}

func main() {
	args := os.Args[1:]
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
	case "launch":
		if err := cmdLaunch(args[1:]); err != nil {
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
		// tab switches / new tabs change the target: reconnect
		if err := fn(c, tokens[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "error (line %d, %s): %v\n", lineNo, tokens[0], err)
			return 1
		}
		if tokens[0] == "tab" || tokens[0] == "open" || tokens[0] == "close" {
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
