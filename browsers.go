package main

// Chromium-family browser detection and launch. Any browser that speaks the
// DevTools protocol works: Chrome, Brave, Edge, Chromium, Vivaldi, Opera.
//
// Since Chromium 136, --remote-debugging-port is ignored for the browser's
// default profile directory, so there are two supported flows:
//
//  1. Real profile: the user enables "Allow remote debugging for this
//     browser instance" at <browser>://inspect/#remote-debugging, which
//     serves DevTools on 127.0.0.1:9222 for their normal browser.
//  2. Dedicated profile: `use-browser launch [name]` starts the browser
//     with a persistent automation profile under the state directory.

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type browser struct {
	Name string // short name used on the command line: chrome, brave, ...
	Path string // executable path
}

// userDataDirs maps each browser's short name to its standard profile
// directory, where a DevToolsActivePort file appears when the
// chrome://inspect remote-debugging toggle is on.
func userDataDirs() map[string]string {
	home, _ := os.UserHomeDir()
	local := os.Getenv("LOCALAPPDATA")
	appdata := os.Getenv("APPDATA")
	switch runtime.GOOS {
	case "windows":
		return map[string]string{
			"chrome":   filepath.Join(local, `Google\Chrome\User Data`),
			"brave":    filepath.Join(local, `BraveSoftware\Brave-Browser\User Data`),
			"edge":     filepath.Join(local, `Microsoft\Edge\User Data`),
			"chromium": filepath.Join(local, `Chromium\User Data`),
			"vivaldi":  filepath.Join(local, `Vivaldi\User Data`),
			"opera":    filepath.Join(appdata, `Opera Software\Opera Stable`),
		}
	case "darwin":
		s := filepath.Join(home, "Library", "Application Support")
		return map[string]string{
			"chrome":   filepath.Join(s, "Google", "Chrome"),
			"brave":    filepath.Join(s, "BraveSoftware", "Brave-Browser"),
			"edge":     filepath.Join(s, "Microsoft Edge"),
			"chromium": filepath.Join(s, "Chromium"),
			"vivaldi":  filepath.Join(s, "Vivaldi"),
			"opera":    filepath.Join(s, "com.operasoftware.Opera"),
		}
	default:
		cfg := filepath.Join(home, ".config")
		return map[string]string{
			"chrome":   filepath.Join(cfg, "google-chrome"),
			"brave":    filepath.Join(cfg, "BraveSoftware", "Brave-Browser"),
			"edge":     filepath.Join(cfg, "microsoft-edge"),
			"chromium": filepath.Join(cfg, "chromium"),
			"vivaldi":  filepath.Join(cfg, "vivaldi"),
			"opera":    filepath.Join(cfg, "opera"),
		}
	}
}

// defaultUserDataDirs returns those directories in browserOrder.
func defaultUserDataDirs() []string {
	byName := userDataDirs()
	var dirs []string
	for _, name := range browserOrder {
		if d := byName[name]; d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// pinnedBrowser is the browser every command should use, or "" for the old
// "whatever is running" behaviour. BU_BROWSER wins over the pin written by
// `use-browser use`, so a single command can override without changing state.
func pinnedBrowser() string {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("BU_BROWSER"))); v != "" {
		return v
	}
	return strings.ToLower(strings.TrimSpace(loadState().Browser))
}

// pinBrowser records the browser every later command should use. A no-op when
// BU_BROWSER is set, so the env var stays a one-shot override.
func pinBrowser(name string) {
	if os.Getenv("BU_BROWSER") != "" {
		return
	}
	st := loadState()
	if st.Browser == name {
		return
	}
	st.Browser = name
	st.Target = "" // the old tab belongs to the old browser
	st.Port = 0    // and so does the old port
	saveState(st)
}

// launchProfileDir is the dedicated automation profile for a browser.
func launchProfileDir(name string) string {
	return filepath.Join(filepath.Dir(stateFile()), "profile-"+name)
}

// freeDebugPort returns the first port from 9222 upward that nothing is
// listening on. Another Chromium holding 9222 (a Brave with the inspect
// toggle on) must not stop us launching a second browser.
func freeDebugPort() int {
	for port := 9222; port < 9322; port++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			continue
		}
		l.Close()
		return port
	}
	return 9222
}

// profileDirs lists every directory that might hold a DevToolsActivePort file:
// our own dedicated launch profiles first, then the browsers' real profiles.
// When a browser is pinned, only that browser's directories are considered —
// otherwise another Chromium running with debugging on (a Brave you left
// open, say) would be picked up instead.
func profileDirs() []string {
	if pin := pinnedBrowser(); pin != "" {
		// dedicated launch profile, cloned real profile, then the real one
		dirs := []string{launchProfileDir(pin), launchProfileDir(pin) + "-clone"}
		if d := userDataDirs()[pin]; d != "" {
			dirs = append(dirs, d)
		}
		return dirs
	}
	var dirs []string
	stateDir := filepath.Dir(stateFile())
	if entries, err := os.ReadDir(stateDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "profile-") {
				dirs = append(dirs, filepath.Join(stateDir, e.Name()))
			}
		}
	}
	return append(dirs, defaultUserDataDirs()...)
}

// inspectURL is the settings page for the real-profile debugging toggle.
// chrome:// works in every Chromium fork, but the fork's own scheme is nicer.
func (b browser) inspectURL() string {
	scheme := map[string]string{"brave": "brave", "edge": "edge", "vivaldi": "vivaldi", "opera": "opera"}[b.Name]
	if scheme == "" {
		scheme = "chrome"
	}
	return scheme + "://inspect/#remote-debugging"
}

func candidatePaths() map[string][]string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pf86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LOCALAPPDATA")
		return map[string][]string{
			"chrome": {
				filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
			},
			"brave": {
				filepath.Join(pf, `BraveSoftware\Brave-Browser\Application\brave.exe`),
				filepath.Join(pf86, `BraveSoftware\Brave-Browser\Application\brave.exe`),
				filepath.Join(local, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			},
			"edge": {
				filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
			},
			"chromium": {
				filepath.Join(local, `Chromium\Application\chrome.exe`),
			},
			"vivaldi": {
				filepath.Join(local, `Vivaldi\Application\vivaldi.exe`),
			},
			"opera": {
				filepath.Join(local, `Programs\Opera\opera.exe`),
			},
		}
	case "darwin":
		return map[string][]string{
			"chrome":   {"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
			"brave":    {"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"},
			"edge":     {"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
			"chromium": {"/Applications/Chromium.app/Contents/MacOS/Chromium"},
			"vivaldi":  {"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi"},
			"opera":    {"/Applications/Opera.app/Contents/MacOS/Opera"},
		}
	default: // linux and friends: rely on PATH
		_ = home
		return map[string][]string{
			"chrome":   {"google-chrome", "google-chrome-stable"},
			"brave":    {"brave-browser", "brave"},
			"edge":     {"microsoft-edge"},
			"chromium": {"chromium", "chromium-browser"},
			"vivaldi":  {"vivaldi"},
			"opera":    {"opera"},
		}
	}
}

var browserOrder = []string{"chrome", "brave", "edge", "chromium", "vivaldi", "opera"}

func detectBrowsers() []browser {
	cands := candidatePaths()
	var found []browser
	for _, name := range browserOrder {
		for _, p := range cands[name] {
			if filepath.IsAbs(p) {
				if _, err := os.Stat(p); err == nil {
					found = append(found, browser{Name: name, Path: p})
					break
				}
			} else if full, err := exec.LookPath(p); err == nil {
				found = append(found, browser{Name: name, Path: full})
				break
			}
		}
	}
	return found
}

func findBrowser(name string) (*browser, error) {
	found := detectBrowsers()
	if len(found) == 0 {
		return nil, fmt.Errorf("no Chromium-family browser found (looked for chrome, brave, edge, chromium, vivaldi, opera)")
	}
	if name == "" {
		if name = pinnedBrowser(); name == "" {
			return &found[0], nil
		}
	}
	for _, b := range found {
		if b.Name == name {
			return &b, nil
		}
	}
	names := ""
	for i, b := range found {
		if i > 0 {
			names += ", "
		}
		names += b.Name
	}
	return nil, fmt.Errorf("%q not found; detected: %s", name, names)
}

// isRunning reports whether the browser's process appears to be running.
func (b browser) isRunning() bool {
	exe := filepath.Base(b.Path)
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+exe, "/NH").Output()
		return err == nil && strings.Contains(strings.ToLower(string(out)), strings.ToLower(exe))
	default:
		err := exec.Command("pgrep", "-f", exe).Run()
		return err == nil
	}
}

// pickBrowser honours the pin first, then prefers a running browser (that is
// "the user's browser"), then falls back to install order.
func pickBrowser() *browser {
	found := detectBrowsers()
	if len(found) == 0 {
		return nil
	}
	if pin := pinnedBrowser(); pin != "" {
		for i := range found {
			if found[i].Name == pin {
				return &found[i]
			}
		}
	}
	for i := range found {
		if found[i].isRunning() {
			return &found[i]
		}
	}
	return &found[0]
}

// connectHelp explains both ways to get a debuggable browser,
// tailored to what is actually installed.
func connectHelp() string {
	found := detectBrowsers()
	if len(found) == 0 {
		return "no Chromium-family browser detected; install Chrome, Brave, or Edge"
	}
	b := pickBrowser()
	s := fmt.Sprintf(`To use your real %s profile (logins intact) with no toggle:
  run: use-browser clone %s
  (copies your profile to a non-default dir, then launches it debuggable)

Or attach to your already-running %s via a one-time toggle:
  use-browser connect %s
  (opens %s; enable "Allow remote debugging for this browser instance")

Or start a separate instance with a dedicated empty profile:
  use-browser launch %s`, b.Name, b.Name, b.Name, b.Name, b.inspectURL(), b.Name)
	if len(found) > 1 {
		s += "\nDetected browsers:"
		for _, f := range found {
			s += " " + f.Name
		}
	}
	return s
}

// cmdConnect attaches to the user's real browser. It opens the browser's
// inspect page where the user enables "Allow remote debugging for this
// browser instance" (required since Chromium 136 for the default profile;
// this is the same flow browser-use automates), then waits for the
// DevTools endpoint to appear.
func cmdConnect(args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	var b *browser
	if name != "" {
		var err error
		if b, err = findBrowser(name); err != nil {
			return err
		}
	} else if b = pickBrowser(); b == nil {
		return fmt.Errorf("no Chromium-family browser detected")
	}
	// Pin before the connected() check: an explicit name has to re-point
	// discovery at that browser, or we would report the previously pinned
	// one as "already connected".
	pinBrowser(b.Name)
	if connected() {
		fmt.Printf("ok already connected to %s (use-browser doctor for details)\n", b.Name)
		return nil
	}
	// Opening the browser binary with the URL lands in the existing
	// instance as a new tab, or starts the browser if it isn't running.
	if err := exec.Command(b.Path, b.inspectURL()).Start(); err != nil {
		return fmt.Errorf("open %s: %v", b.inspectURL(), err)
	}
	fmt.Printf("opened %s in %s\n", b.inspectURL(), b.Name)
	fmt.Println(`enable "Allow remote debugging for this browser instance" (waiting up to 90s)...`)
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if connected() {
			fmt.Printf("ok connected to %s (real profile)\n", b.Name)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out; tick the checkbox in %s and rerun: use-browser connect %s", b.Name, b.Name)
}

// cmdLaunch starts a detected browser with a persistent automation profile.
// Chromium 136+ requires a non-default --user-data-dir for flag-based
// debugging, so this profile lives under the state directory and keeps
// its logins between runs.
func cmdLaunch(args []string) error {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	b, err := findBrowser(name)
	if err != nil {
		return err
	}
	pinBrowser(b.Name)
	if connected() {
		fmt.Printf("ok already connected to %s (use-browser doctor for details)\n", b.Name)
		return nil
	}
	profile := launchProfileDir(b.Name)
	os.MkdirAll(profile, 0o755)
	port := freeDebugPort()
	savePort(port)
	cmd := exec.Command(b.Path,
		"--remote-debugging-port="+strconv.Itoa(port),
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %v", b.Path, err)
	}
	// wait for the DevTools endpoint
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if connected() {
			fmt.Printf("ok %s pid=%d port=%d profile=%s\n", b.Name, cmd.Process.Pid, port, profile)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%s started (pid %d) but the DevTools endpoint never came up on :%d", b.Name, cmd.Process.Pid, port)
}

// cmdUse pins the browser that every later command talks to, so a second
// Chromium being open (or open first) can't hijack the connection.
func cmdUse(args []string) error {
	if len(args) == 0 {
		pin := pinnedBrowser()
		if pin == "" {
			fmt.Println("browser: auto (first running, else install order)")
		} else {
			src := "state file"
			if os.Getenv("BU_BROWSER") != "" {
				src = "BU_BROWSER"
			}
			fmt.Printf("browser: %s (from %s)\n", pin, src)
		}
		fmt.Print("installed:")
		for _, b := range detectBrowsers() {
			fmt.Print(" " + b.Name)
		}
		fmt.Println("\nset with: use-browser use <name>   |   unset with: use-browser use auto")
		return nil
	}
	name := strings.ToLower(args[0])
	if name == "auto" || name == "none" || name == "--clear" {
		saveState(buState{})
		fmt.Println("ok browser: auto")
		return nil
	}
	b, err := findBrowser(name)
	if err != nil {
		return err
	}
	pinBrowser(b.Name)
	if os.Getenv("BU_BROWSER") != "" {
		fmt.Printf("warning: BU_BROWSER=%s is set and overrides this pin for the current shell\n", os.Getenv("BU_BROWSER"))
	}
	fmt.Printf("ok browser: %s (%s)\n", b.Name, b.Path)
	if !connected() {
		fmt.Printf("not connected yet — start it with: use-browser clone %s   (or: use-browser launch %s)\n", b.Name, b.Name)
	}
	return nil
}

// portOwnedBy reports whether the process listening on a local port is the
// named browser. This is what keeps a pin honest: Brave sitting on 9222 with
// the inspect toggle on must not be mistaken for the Chrome we pinned.
// Permissive on failure — if ownership can't be determined, don't block.
func portOwnedBy(port, name string) bool {
	b, err := findBrowser(name)
	if err != nil {
		return true
	}
	exe := strings.ToLower(filepath.Base(b.Path))
	pid := listenerPID(port)
	if pid == "" {
		return true
	}
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("tasklist", "/FI", "PID eq "+pid, "/NH", "/FO", "CSV").Output()
		if err != nil {
			return true
		}
		return strings.Contains(strings.ToLower(string(out)), exe)
	default:
		out, err := exec.Command("ps", "-p", pid, "-o", "comm=").Output()
		if err != nil {
			return true
		}
		return strings.Contains(strings.ToLower(string(out)), exe)
	}
}

// listenerPID returns the pid listening on a local TCP port, or "".
func listenerPID(port string) string {
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 5 || !strings.EqualFold(f[3], "LISTENING") {
				continue
			}
			if strings.HasSuffix(f[1], ":"+port) {
				return f[4]
			}
		}
	default:
		out, err := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN", "-t").Output()
		if err != nil {
			return ""
		}
		if f := strings.Fields(string(out)); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}
