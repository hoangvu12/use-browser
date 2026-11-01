package main

// Extension bridge: drive the user's real, running browser through the
// use-browser bridge extension (extension/), with no debug port, no profile
// copy and no per-connection "Allow remote debugging?" dialog.
//
//   use-browser <cmd> --TCP 127.0.0.1, token--> native host --stdio--> extension
//
// The native host is this same binary. The browser starts it when the
// extension calls connectNative, passing the extension origin as the first
// argument; it lives exactly as long as that port. It listens on a loopback
// port, records {port, token} in a bridge file, and relays CDP JSON between
// CLI connections and the extension, tagging each message with the CLI
// connection it belongs to. The extension answers Target.* itself and sends
// page commands to chrome.debugger, so the CLI side is the ordinary cdpClient
// with a different pipe under it.
//
// The CLI side is raw length-prefixed frames, not WebSocket, on purpose: a web
// page can open a WebSocket to 127.0.0.1 but cannot speak raw TCP, so no site
// can even reach the token check.

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed extension
var extensionFiles embed.FS

const (
	nativeHostName = "com.usebrowser.bridge"
	// Fixed by the "key" in extension/manifest.json, so an unpacked install
	// gets the same id on every machine and the host manifest can name it.
	extensionID = "blcbodpjbiimmoenmdneeacdogpglgne"
	// The browser refuses host-to-extension messages over 1 MB.
	maxToExtension = 1024 * 1024
)

// ---- framing (native messaging and the CLI link share it) ----

// Native messaging uses native byte order; every platform Chromium ships on
// is little-endian.
func writeFrame(w io.Writer, b []byte) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > 256<<20 {
		return nil, fmt.Errorf("frame of %d bytes is too large", n)
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

// msgConn is what cdpClient needs from its transport: one JSON message in,
// one out. wsConn (a real DevTools endpoint) and frameConn (the bridge) both
// provide it.
type msgConn interface {
	WriteText([]byte) error
	ReadMessage(deadline time.Time) ([]byte, error)
	Close() error
}

type frameConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func (f *frameConn) WriteText(b []byte) error { return writeFrame(f.conn, b) }

func (f *frameConn) ReadMessage(deadline time.Time) ([]byte, error) {
	f.conn.SetReadDeadline(deadline)
	defer f.conn.SetReadDeadline(time.Time{})
	return readFrame(f.br)
}

func (f *frameConn) Close() error { return f.conn.Close() }

// ---- bridge files ----

// bridgeInfo is what a running native host publishes about itself. One file
// per browser profile that has the extension, since each profile runs its own
// extension instance and therefore its own host.
type bridgeInfo struct {
	PID      int    `json:"pid"`
	Port     int    `json:"port"`
	Token    string `json:"token"`
	Browser  string `json:"browser"`
	Instance string `json:"instance"`
	Version  string `json:"version"`
	// Build is the content hash the loaded extension was written with (its
	// build.json); "" for builds from before the hash existed.
	Build string `json:"build"`
	path  string
}

func bridgesDir() string { return filepath.Join(stateDir(), "bridges") }

// liveBridges reads the bridge files, deleting any whose host is gone (the
// browser closed, or killed the host without letting it clean up).
func liveBridges() []bridgeInfo {
	paths, _ := filepath.Glob(filepath.Join(bridgesDir(), "*.json"))
	var out []bridgeInfo
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var bi bridgeInfo
		if json.Unmarshal(b, &bi) != nil || bi.Port == 0 {
			continue
		}
		if !portAlive(strconv.Itoa(bi.Port)) {
			os.Remove(p)
			continue
		}
		bi.path = p
		out = append(out, bi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// pickBridge chooses the bridge for this command: the pinned browser's, and
// only when that is unambiguous. BU_EXT names an instance outright; BU_EXT=off
// ignores bridges altogether.
func pickBridge() (*bridgeInfo, error) {
	want := strings.TrimSpace(os.Getenv("BU_EXT"))
	if want == "off" || want == "0" {
		return nil, nil
	}
	pin := pinnedBrowser()
	var hits []bridgeInfo
	for _, b := range liveBridges() {
		switch {
		case want != "" && b.Instance != want:
		case pin != "" && b.Browser != pin:
		default:
			hits = append(hits, b)
		}
	}
	switch len(hits) {
	case 0:
		return nil, nil
	case 1:
		return &hits[0], nil
	}
	var names []string
	for _, h := range hits {
		names = append(names, h.Browser)
	}
	if len(distinctStrings(names)) > 1 {
		return nil, ambiguousBrowsers(distinctStrings(names))
	}
	s := fmt.Sprintf("%d %s profiles are running the use-browser extension; pick one with BU_EXT=<instance>:", len(hits), hits[0].Browser)
	for _, h := range hits {
		s += "\n  BU_EXT=" + h.Instance
	}
	return nil, fmt.Errorf("%s", s)
}

func distinctStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// dialBridge opens a CLI connection to a native host and authenticates.
func dialBridge(b *bridgeInfo) (*frameConn, error) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(b.Port), 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("extension bridge on :%d: %v", b.Port, err)
	}
	hello, _ := json.Marshal(map[string]string{"token": b.Token})
	if err := writeFrame(conn, hello); err != nil {
		conn.Close()
		return nil, err
	}
	return &frameConn{conn: conn, br: bufio.NewReaderSize(conn, 64*1024)}, nil
}

// ---- native host ----

// isNativeHostLaunch recognizes the browser starting us as a native messaging
// host: the first argument is the calling extension's origin.
func isNativeHostLaunch(args []string) bool {
	return len(args) > 0 && strings.HasPrefix(args[0], "chrome-extension://")
}

type nativeHost struct {
	out     io.Writer
	outMu   sync.Mutex
	mu      sync.Mutex
	clients map[int]chan []byte
	nextID  int
	token   string
	log     *os.File
}

func (h *nativeHost) toExtension(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h.outMu.Lock()
	defer h.outMu.Unlock()
	return writeFrame(h.out, b)
}

func (h *nativeHost) logf(format string, a ...any) {
	if h.log != nil {
		fmt.Fprintf(h.log, time.Now().Format("15:04:05 ")+format+"\n", a...)
	}
}

// runNativeHost serves until the browser closes our stdin, which it does when
// the extension's port goes away (worker stopped, extension reloaded, browser
// closed).
func runNativeHost(origin string) int {
	os.MkdirAll(bridgesDir(), 0o755)
	h := &nativeHost{out: os.Stdout, clients: map[int]chan []byte{}}
	h.log, _ = os.Create(filepath.Join(bridgesDir(), "host.log"))
	h.logf("started by %s", origin)

	in := bufio.NewReaderSize(os.Stdin, 64*1024)
	first, err := readFrame(in)
	if err != nil {
		h.logf("no hello: %v", err)
		return 1
	}
	var hello struct {
		Hello struct {
			Browser  string `json:"browser"`
			Instance string `json:"instance"`
			Version  string `json:"version"`
			Build    string `json:"build"`
		} `json:"hello"`
	}
	json.Unmarshal(first, &hello)
	if hello.Hello.Browser == "" || !validSessionName(hello.Hello.Instance) {
		h.logf("bad hello: %s", first)
		return 1
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.logf("listen: %v", err)
		return 1
	}
	tok := make([]byte, 16)
	rand.Read(tok)
	h.token = hex.EncodeToString(tok)
	info := bridgeInfo{
		PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Token: h.token,
		Browser: hello.Hello.Browser, Instance: hello.Hello.Instance, Version: hello.Hello.Version,
		Build: hello.Hello.Build,
	}
	path := filepath.Join(bridgesDir(), info.Browser+"-"+info.Instance+".json")
	b, _ := json.Marshal(info)
	// The file holds the token, so only this user may read it.
	if err := os.WriteFile(path, b, 0o600); err != nil {
		h.logf("write %s: %v", path, err)
		return 1
	}
	defer os.Remove(path)
	h.logf("%s instance %s listening on :%d", info.Browser, info.Instance, info.Port)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go h.serveClient(conn)
		}
	}()

	for {
		msg, err := readFrame(in)
		if err != nil {
			h.logf("extension gone: %v", err)
			ln.Close()
			return 0
		}
		var env struct {
			C int             `json:"c"`
			M json.RawMessage `json:"m"`
		}
		if json.Unmarshal(msg, &env) != nil || env.M == nil {
			continue
		}
		h.mu.Lock()
		ch := h.clients[env.C]
		h.mu.Unlock()
		if ch != nil {
			select {
			case ch <- env.M:
			case <-time.After(5 * time.Second):
				h.logf("client %d is not reading; dropped a message", env.C)
			}
		}
	}
}

func (h *nativeHost) serveClient(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 64*1024)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := readFrame(br)
	conn.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	var auth struct {
		Token string `json:"token"`
	}
	json.Unmarshal(first, &auth)
	if subtle.ConstantTimeCompare([]byte(auth.Token), []byte(h.token)) != 1 {
		h.logf("rejected a connection with a bad token")
		return
	}

	h.mu.Lock()
	h.nextID++
	id := h.nextID
	ch := make(chan []byte, 256)
	h.clients[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, id)
		h.mu.Unlock()
		h.toExtension(map[string]any{"c": id, "close": true})
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case m := <-ch:
				if writeFrame(conn, m) != nil {
					conn.Close()
					return
				}
			case <-done:
				return
			}
		}
	}()

	for {
		msg, err := readFrame(br)
		if err != nil {
			return
		}
		if len(msg)+64 > maxToExtension {
			// Answer it here: the browser would kill the whole host over it.
			var req struct {
				ID int `json:"id"`
			}
			json.Unmarshal(msg, &req)
			e, _ := json.Marshal(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": "command is larger than the 1 MB the extension bridge can carry"}})
			ch <- e
			continue
		}
		if err := h.toExtension(map[string]any{"c": id, "m": json.RawMessage(msg)}); err != nil {
			return
		}
	}
}

// ---- use-browser ext ----

func cmdExt(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "install":
		return extInstall(args)
	case "uninstall":
		return extUninstall()
	case "status":
		return extStatus(args)
	case "path":
		fmt.Println(extensionDir())
		return nil
	}
	return fmt.Errorf("usage: use-browser ext [install [browser] [--no-wait] | status [browser] | path | uninstall]")
}

func extensionDir() string { return filepath.Join(stateDir(), "extension") }

func hostManifestPath() string {
	return filepath.Join(stateDir(), "native-host", nativeHostName+".json")
}

// hostManifestDirs is where each browser looks for native host manifests on
// macOS and Linux: a NativeMessagingHosts folder in its user data root.
func hostManifestDirs() map[string]string {
	dirs := map[string]string{}
	for name, root := range userDataDirs() {
		if name != "opera" {
			dirs[name] = filepath.Join(root, "NativeMessagingHosts")
		}
	}
	return dirs
}

// registryKeys is where each browser looks on Windows. Chrome documents only
// its own; Chromium-based browsers read their vendor key, and Brave and
// Vivaldi also fall back to Chrome's, so writing every one is harmless.
var registryKeys = map[string]string{
	"chrome":   `HKCU\Software\Google\Chrome\NativeMessagingHosts\` + nativeHostName,
	"chromium": `HKCU\Software\Chromium\NativeMessagingHosts\` + nativeHostName,
	"edge":     `HKCU\Software\Microsoft\Edge\NativeMessagingHosts\` + nativeHostName,
	"brave":    `HKCU\Software\BraveSoftware\Brave-Browser\NativeMessagingHosts\` + nativeHostName,
	"vivaldi":  `HKCU\Software\Vivaldi\NativeMessagingHosts\` + nativeHostName,
}

func extInstall(args []string) error {
	name, wait := "", true
	for _, a := range args {
		if a == "--no-wait" {
			wait = false
		} else {
			name = strings.ToLower(a)
		}
	}
	b, err := findBrowser(name)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(exe, "go-build") {
		return fmt.Errorf("%s is a temporary `go run` binary; build use-browser and run install from the built binary", exe)
	}

	// 1. The extension, unpacked where the browser can load it.
	dir := extensionDir()
	if err := writeExtensionDir(); err != nil {
		return err
	}

	// 2. The native host manifest, pointing at this binary.
	manifest, _ := json.MarshalIndent(map[string]any{
		"name":            nativeHostName,
		"description":     "use-browser extension bridge",
		"path":            exe,
		"type":            "stdio",
		"allowed_origins": []string{"chrome-extension://" + extensionID + "/"},
	}, "", "  ")
	mp := hostManifestPath()
	os.MkdirAll(filepath.Dir(mp), 0o755)
	if err := os.WriteFile(mp, manifest, 0o644); err != nil {
		return err
	}

	// 3. Tell the browsers where it is.
	if runtime.GOOS == "windows" {
		for _, key := range registryKeys {
			if out, err := exec.Command("reg", "add", key, "/ve", "/t", "REG_SZ", "/d", mp, "/f").CombinedOutput(); err != nil {
				return fmt.Errorf("reg add %s: %v %s", key, err, strings.TrimSpace(string(out)))
			}
		}
	} else {
		for _, d := range hostManifestDirs() {
			if _, err := os.Stat(filepath.Dir(d)); err != nil {
				continue // browser not installed
			}
			os.MkdirAll(d, 0o755)
			if err := os.WriteFile(filepath.Join(d, nativeHostName+".json"), manifest, 0o644); err != nil {
				return err
			}
		}
	}
	pinBrowser(b.Name)
	fmt.Printf("ok native host registered (%s)\n", exe)

	if br := bridgeFor(b.Name); br != nil {
		currentBridge(br)
		fmt.Printf("ok %s is connected through the extension\n", b.Name)
		return nil
	}

	// 4. The one step only a human can do: load it into the real profile.
	// Browsers refuse to open their own pages from the command line (Brave
	// shows its start page instead), but an https page opens fine, so the
	// browser gets the guide and the file picker gets the path.
	clip := ""
	if copyToClipboard(dir) == nil {
		clip = " (the folder path is on the clipboard)"
	}
	opened := exec.Command(b.Path, guideURL).Start() == nil

	// An agent ran this: it cannot click, and its user cannot see this
	// output. Hand it the words to relay and the command that confirms them,
	// instead of blocking its shell for five minutes.
	if !wait || !stdoutIsTerminal() {
		fmt.Printf("ok extension written to %s%s\n", dir, clip)
		if opened {
			fmt.Printf("ok opened the setup guide in %s: %s\n", b.Name, guideURL)
		}
		fmt.Printf("only the user can do the last step, once. Tell them:\n%s\nthen run: use-browser ext status %s   (exit 0 once it is connected)\n", loadSteps(b.Name), b.Name)
		return nil
	}
	fmt.Printf("load the extension once%s:\n%s\nwaiting up to 5 minutes for it to connect...\n", clip, loadSteps(b.Name))
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if br := bridgeFor(b.Name); br != nil {
			fmt.Printf("ok %s connected through the extension (instance %s); your real profile, no prompts\n", b.Name, br.Instance)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("the extension did not connect. Find out why: use-browser ext status %s", b.Name)
}

const guideURL = "https://github.com/hoangvu12/use-browser#set-up-the-extension"

var displayNames = map[string]string{
	"chrome": "Chrome", "brave": "Brave", "edge": "Edge",
	"chromium": "Chromium", "vivaldi": "Vivaldi", "opera": "Opera",
}

// loadSteps is the manual step, worded for a person: an agent pastes it as is.
func loadSteps(name string) string {
	return fmt.Sprintf(`  1. In %s, type %s://extensions in the address bar
  2. Turn on "Developer mode" (top right)
  3. Click "Load unpacked" and choose this folder:
     %s`, displayNames[name], schemeFor(name), extensionDir())
}

func reloadStep(name string) string {
	return fmt.Sprintf(`in %s, open %s://extensions and click the reload icon on "use-browser bridge"`, displayNames[name], schemeFor(name))
}

func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// bundledExtBuild hashes the extension embedded in this binary. The folder
// gets it as build.json and the loaded extension reports it back, so any
// change to the extension is noticed without anyone bumping a version.
func bundledExtBuild() string {
	h := sha256.New()
	fs.WalkDir(extensionFiles, "extension", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			data, _ := extensionFiles.ReadFile(p)
			fmt.Fprintf(h, "%s %d\n", p, len(data))
			h.Write(data)
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeExtensionDir writes the embedded extension where the browser loads it
// from, stamped with its build hash.
func writeExtensionDir() error {
	dir := extensionDir()
	err := fs.WalkDir(extensionFiles, "extension", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "extension")))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := extensionFiles.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("write extension: %v", err)
	}
	stamp, _ := json.Marshal(map[string]string{"build": bundledExtBuild()})
	return os.WriteFile(filepath.Join(dir, "build.json"), stamp, 0o644)
}

// updateExtension brings the extension a browser is running up to the one in
// this binary: rewrite its folder, have it reload itself from there, and wait
// for the bridge it comes back with. Unpacked extensions reload from disk,
// which is what makes a release reach the browser with nobody clicking.
func updateExtension(b *bridgeInfo) (*bridgeInfo, error) {
	if err := writeExtensionDir(); err != nil {
		return nil, err
	}
	fc, err := dialBridge(b)
	if err != nil {
		return nil, err
	}
	c := &cdpClient{ws: fc}
	_, err = c.browserCall("UseBrowser.reload", nil)
	c.Close()
	if err != nil {
		return nil, err // an extension from before self-update existed
	}
	want := bundledExtBuild()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, nb := range liveBridges() {
			if nb.Instance == b.Instance && nb.Build == want {
				return &nb, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, fmt.Errorf("it did not reconnect within 15s after reloading")
}

// currentBridge updates an outdated extension on the spot. When that is not
// possible it says what to tell the user, and the old one keeps working.
func currentBridge(b *bridgeInfo) *bridgeInfo {
	if b.Build == bundledExtBuild() {
		return b
	}
	nb, err := updateExtension(b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: the use-browser extension in %s is out of date and could not update itself (%v). Tell the user: %s\n",
			b.Browser, err, reloadStep(b.Browser))
		return b
	}
	fmt.Fprintf(os.Stderr, "note: updated the use-browser extension in %s\n", b.Browser)
	return nb
}

// bundledExtVersion is the version of the extension embedded in this binary.
func bundledExtVersion() string {
	b, _ := extensionFiles.ReadFile("extension/manifest.json")
	var m struct {
		Version string `json:"version"`
	}
	json.Unmarshal(b, &m)
	return m.Version
}

func copyToClipboard(s string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("clip")
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		return fmt.Errorf("no clipboard tool")
	}
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

func bridgeFor(name string) *bridgeInfo {
	for _, b := range liveBridges() {
		if b.Browser == name {
			return &b
		}
	}
	return nil
}

func schemeFor(name string) string {
	switch name {
	case "edge", "brave", "vivaldi", "opera":
		return name
	}
	return "chrome"
}

// extStatus says whether a browser is reachable through the extension and,
// when it is not, exactly which step is missing and what to tell the user.
// Exit status 0 means connected, so an agent can poll it.
func extStatus(args []string) error {
	var names []string
	switch {
	case len(args) > 0:
		b, err := findBrowser(strings.ToLower(args[0]))
		if err != nil {
			return err
		}
		names = []string{b.Name}
	case pinnedBrowser() != "":
		names = []string{pinnedBrowser()}
	default:
		for _, b := range detectBrowsers() {
			names = append(names, b.Name)
		}
	}
	ok := false
	for _, name := range names {
		if diagnoseExt(name) {
			ok = true
		}
	}
	if !ok {
		return fmt.Errorf("not connected through the extension yet")
	}
	return nil
}

// diagnoseExt prints one browser's extension state, most specific cause
// first, and reports whether commands can use it right now.
func diagnoseExt(name string) bool {
	if br := bridgeFor(name); br != nil {
		br = currentBridge(br)
		if err := probeBridge(br); err != nil {
			if strings.Contains(err.Error(), "turned off") {
				fmt.Printf("off: the user turned use-browser off in %s. Do not work around it. If they want it back, tell them: click the puzzle-piece Extensions button in the toolbar, then \"use-browser bridge\" (pinning it there keeps the switch one click away).\n", displayNames[name])
			} else {
				fmt.Printf("error: %s's extension is running but not answering: %v\n", name, err)
			}
			return false
		}
		fmt.Printf("ok %s connected through the extension (instance %s, v%s)\n", name, br.Instance, br.Version)
		return true
	}
	if problem := hostProblem(name); problem != "" {
		fmt.Printf("%s: %s. Run: use-browser ext install %s\n", name, problem, name)
		return false
	}
	loaded, enabled := extInProfiles(name)
	b, _ := findBrowser(name)
	switch {
	case len(loaded) == 0:
		fmt.Printf("%s: the extension is not loaded yet. Tell the user:\n%s\n(If they just did, wait a few seconds and run this again.)\n", name, loadSteps(name))
	case len(enabled) == 0:
		fmt.Printf("%s: the extension is loaded (%s) but switched off. Tell the user: in %s, open %s://extensions and turn on \"use-browser bridge\".\n",
			name, strings.Join(loaded, ", "), displayNames[name], schemeFor(name))
	case b != nil && !b.isRunning():
		fmt.Printf("%s: the extension is installed but %s is not running. It connects when %s starts; ask the user to open it.\n", name, displayNames[name], displayNames[name])
	default:
		fmt.Printf("%s: the extension is loaded but could not start use-browser.%s Run: use-browser ext install %s\nThen tell the user: %s. If its toolbar icon shows a red \"!\", hovering it shows the reason.\n",
			name, lastHostLog(), name, reloadStep(name))
	}
	return false
}

// probeBridge makes one round trip through the extension. It is how a bridge
// that is up but switched off by the user shows itself.
func probeBridge(b *bridgeInfo) error {
	fc, err := dialBridge(b)
	if err != nil {
		return err
	}
	c := &cdpClient{ws: fc}
	defer c.Close()
	_, err = c.browserCall("Browser.getVersion", nil)
	return err
}

// hostProblem checks the registration a browser needs to start use-browser.
func hostProblem(name string) string {
	b, err := os.ReadFile(hostManifestPath())
	if err != nil {
		return "the native host is not registered"
	}
	var m struct {
		Path string `json:"path"`
	}
	json.Unmarshal(b, &m)
	if _, err := os.Stat(m.Path); err != nil {
		return fmt.Sprintf("the native host points at %s, which no longer exists", m.Path)
	}
	if runtime.GOOS == "windows" {
		if key, ok := registryKeys[name]; ok && exec.Command("reg", "query", key, "/ve").Run() != nil {
			return "the native host is not registered for this browser"
		}
	}
	return ""
}

// extInProfiles reads the browser's own records of which profiles have the
// extension, and which of those have it enabled.
func extInProfiles(name string) (loaded, enabled []string) {
	dirs, _ := filepath.Glob(filepath.Join(userDataDirs()[name], "*"))
	for _, d := range dirs {
		base := filepath.Base(d)
		if base != "Default" && !strings.HasPrefix(base, "Profile ") {
			continue
		}
		for _, f := range []string{"Secure Preferences", "Preferences"} {
			b, err := os.ReadFile(filepath.Join(d, f))
			if err != nil {
				continue
			}
			var prefs struct {
				Extensions struct {
					Settings map[string]struct {
						DisableReasons json.RawMessage `json:"disable_reasons"`
						State          *int            `json:"state"`
					} `json:"settings"`
				} `json:"extensions"`
			}
			if json.Unmarshal(b, &prefs) != nil {
				continue
			}
			s, ok := prefs.Extensions.Settings[extensionID]
			if !ok {
				continue
			}
			loaded = append(loaded, base)
			// disable_reasons is a bit mask in older builds, a list in newer.
			r := strings.TrimSpace(string(s.DisableReasons))
			off := r != "" && r != "0" && r != "[]" && r != "null"
			if s.State != nil && *s.State == 0 {
				off = true
			}
			if !off {
				enabled = append(enabled, base)
			}
			break
		}
	}
	return loaded, enabled
}

// lastHostLog is the host's last log line, which names the failure when the
// browser did start use-browser but it gave up.
func lastHostLog() string {
	b, err := os.ReadFile(filepath.Join(bridgesDir(), "host.log"))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return " Last host log: " + strings.TrimSpace(lines[len(lines)-1]) + "."
}

func extUninstall() error {
	if runtime.GOOS == "windows" {
		for _, key := range registryKeys {
			exec.Command("reg", "delete", key, "/f").Run()
		}
	} else {
		for _, d := range hostManifestDirs() {
			os.Remove(filepath.Join(d, nativeHostName+".json"))
		}
	}
	os.Remove(hostManifestPath())
	os.RemoveAll(extensionDir())
	fmt.Println("ok native host unregistered; remove \"use-browser bridge\" from your browser's extensions page")
	return nil
}
