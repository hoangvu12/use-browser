package main

// Clone flow: copy the user's real profile into a non-default user-data-dir,
// then launch the browser against the copy with --remote-debugging-port baked
// in. This gives an automated browser that carries the user's real logins and
// cookies, without the chrome://inspect toggle.
//
// It works because Chromium 136 only ignores --remote-debugging-port for a
// browser's *default* profile directory. A copy lives at a different path, so
// flag-based debugging is allowed there. This is the same trick browser-use
// falls back to (see browser-use issue #1520): duplicate the profile, drive
// the duplicate. The original profile is only ever read, never launched with
// debugging, so the user's day-to-day browser keeps its own encryption key.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// realUserDataDir returns the user-data-dir of a browser's normal install,
// i.e. the directory that holds "Local State" and the profile subdirectories.
func realUserDataDir(name string) string {
	return userDataDirs()[name]
}

// listProfiles returns the profile subdirectory names inside a user-data-dir
// (the ones that contain a "Preferences" file), e.g. Default, "Profile 1".
func listProfiles(userDataDir string) []string {
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(userDataDir, e.Name(), "Preferences")); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

// profileInfo is one real profile inside a browser's user-data-dir: the
// directory name you pass to `clone --profile`, plus the label the user
// actually recognises from the browser's own profile menu.
type profileInfo struct {
	Dir      string
	Name     string
	UserName string
}

// readProfileInfo reads the display names a Chromium browser keeps in its
// "Local State" file. Falls back to bare directory names when that file is
// missing or unreadable, because the directory name is what clone needs.
func readProfileInfo(userDataDir string) []profileInfo {
	names := map[string]struct{ Name, UserName string }{}
	if b, err := os.ReadFile(filepath.Join(userDataDir, "Local State")); err == nil {
		var ls struct {
			Profile struct {
				InfoCache map[string]struct {
					Name     string `json:"name"`
					UserName string `json:"user_name"`
				} `json:"info_cache"`
			} `json:"profile"`
		}
		if json.Unmarshal(b, &ls) == nil {
			for dir, v := range ls.Profile.InfoCache {
				names[dir] = struct{ Name, UserName string }{v.Name, v.UserName}
			}
		}
	}
	var out []profileInfo
	for _, dir := range listProfiles(userDataDir) {
		out = append(out, profileInfo{Dir: dir, Name: names[dir].Name, UserName: names[dir].UserName})
	}
	return out
}

// cmdProfiles lists the real profiles of a browser, so that `clone --profile`
// is discoverable instead of being something you learn from an error message.
func cmdProfiles(args []string) error {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = strings.ToLower(args[0])
	}
	b, err := findBrowser(name)
	if err != nil {
		return err
	}
	src := realUserDataDir(b.Name)
	if src == "" {
		return fmt.Errorf("don't know where %s stores its profiles on this OS", b.Name)
	}
	profiles := readProfileInfo(src)
	if len(profiles) == 0 {
		return fmt.Errorf("no %s profiles found in %s (has %s ever been run?)", b.Name, src, b.Name)
	}
	fmt.Printf("%s profiles (%s):\n", b.Name, src)
	for _, p := range profiles {
		label := p.Name
		if label == "" {
			label = "(no name)"
		}
		line := fmt.Sprintf("  %-12s %q", p.Dir, label)
		if p.UserName != "" {
			line += "  " + p.UserName
		}
		fmt.Println(line)
	}
	fmt.Printf("copy one and drive it: use-browser clone %s --profile %s\n", b.Name, strconv.Quote(profiles[0].Dir))
	return nil
}

// cacheDirs are profile subdirectories that only hold regenerable caches.
// Skipping them keeps the clone small and avoids copying gigabytes of junk;
// none of them are needed to preserve logins.
var cacheDirs = map[string]bool{
	"Cache": true, "Code Cache": true, "GPUCache": true, "DawnCache": true,
	"DawnGraphiteCache": true, "DawnWebGPUCache": true, "GrShaderCache": true,
	"ShaderCache": true, "CacheStorage": true, "ScriptCache": true,
	"Service Worker": true, "component_crx_cache": true, "extensions_crx_cache": true,
}

// copyTree copies src into dst, skipping any path that passes through a
// cache directory. File-level errors (e.g. a lock held by a running browser)
// are counted and skipped rather than aborting the whole copy.
func copyTree(src, dst string) (skipped int) {
	filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			skipped++
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			if cacheDirs[d.Name()] {
				return filepath.SkipDir
			}
			os.MkdirAll(filepath.Join(dst, rel), 0o755)
			return nil
		}
		if err := copyFile(path, filepath.Join(dst, rel)); err != nil {
			skipped++
		}
		return nil
	})
	return skipped
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	os.MkdirAll(filepath.Dir(dst), 0o755)
	out, err := os.Create(dst)
	if err != nil {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// Carry the modification time across. Without it every later sync sees
	// each file as "changed" and copies the whole profile again.
	if fi, err := os.Stat(src); err == nil {
		os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
	return nil
}

// dbSuffixes are the sidecars Chromium's SQLite databases leave next to the
// database file itself.
var dbSuffixes = []string{"-wal", "-shm", "-journal"}

// groupKey names the SQLite group a file belongs to. "Cookies", "Cookies-wal"
// and "Cookies-shm" all answer "Cookies". Files with no sidecar suffix answer
// with their own name, which is harmless: a group of one.
func groupKey(name string) string {
	for _, suf := range dbSuffixes {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return name
}

type syncStat struct {
	Copied  int
	Skipped int
	Bytes   int64
}

// syncTree copies only what differs between src and dst, comparing size and
// modification time. A SQLite database and its sidecars move as one group: a
// fresh Cookies file next to a stale Cookies-wal describes a transaction that
// its journal no longer explains, and the browser reads it as corrupt.
func syncTree(src, dst string) syncStat {
	var st syncStat
	// changed[dir][group] = true once any member of that group differs.
	changed := map[string]map[string]bool{}
	type entry struct{ rel, dir, name string }
	var files []entry

	filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			st.Skipped++
			return nil
		}
		if d.IsDir() {
			if cacheDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return nil
		}
		dir := filepath.Dir(rel)
		files = append(files, entry{rel, dir, d.Name()})
		si, err := d.Info()
		if err != nil {
			st.Skipped++
			return nil
		}
		di, err := os.Stat(filepath.Join(dst, rel))
		if err == nil && di.Size() == si.Size() && di.ModTime().Equal(si.ModTime()) {
			return nil
		}
		if changed[dir] == nil {
			changed[dir] = map[string]bool{}
		}
		changed[dir][groupKey(d.Name())] = true
		return nil
	})

	present := map[string]bool{}
	for _, f := range files {
		present[f.rel] = true
		if !changed[f.dir][groupKey(f.name)] {
			continue
		}
		if err := copyFile(filepath.Join(src, f.rel), filepath.Join(dst, f.rel)); err != nil {
			st.Skipped++
			continue
		}
		st.Copied++
		if fi, err := os.Stat(filepath.Join(dst, f.rel)); err == nil {
			st.Bytes += fi.Size()
		}
	}
	// A sidecar the source no longer has must not survive in the copy, or the
	// browser replays a journal against a database that moved past it.
	for _, f := range files {
		if !changed[f.dir][groupKey(f.name)] {
			continue
		}
		for _, suf := range dbSuffixes {
			side := filepath.Join(f.dir, groupKey(f.name)+suf)
			if !present[side] {
				os.Remove(filepath.Join(dst, side))
			}
		}
	}
	return st
}

// processAlive reports whether a pid is still running.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// profileInUse reports whether a browser currently owns a user-data-dir.
// Writing into a live profile corrupts its databases, and launching a second
// browser against it does not start anything: Chromium hands the command to
// the instance that already holds the directory, and our new process exits.
//
// DevToolsActivePort alone is not enough — Chrome and Brave often never write
// it — so the singleton lock is the real signal.
func profileInUse(dir string) bool {
	if port, _ := activePort(dir); port != "" && portAlive(port) {
		return true
	}
	// Windows: Chromium holds "lockfile" open exclusively for as long as it
	// runs. The file outlives a crash but becomes openable again, so a stale
	// one does not read as in-use.
	if f, err := os.OpenFile(filepath.Join(dir, "lockfile"), os.O_RDWR, 0); err == nil {
		f.Close()
	} else if !os.IsNotExist(err) {
		return true
	}
	// POSIX: SingletonLock is a symlink to "<hostname>-<pid>".
	if target, err := os.Readlink(filepath.Join(dir, "SingletonLock")); err == nil {
		if i := strings.LastIndexByte(target, '-'); i >= 0 {
			if pid, err := strconv.Atoi(target[i+1:]); err == nil && processAlive(pid) {
				return true
			}
		}
	}
	return false
}

// cookiesLocked reports whether the cookie database cannot be read. A missing
// file is not locked: there is simply nothing to copy.
func cookiesLocked(path string) bool {
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		return false
	}
	return !os.IsNotExist(err)
}

// closeBrowser asks a running browser to quit, then waits for it to let go of
// its cookie database. It sends a close request rather than killing the
// process: Chromium has to flush SQLite and write its session file, or the
// user loses their tabs and the copy taken next is torn.
func closeBrowser(b *browser, cookieDB string, timeout time.Duration) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		// No /F. That sends WM_CLOSE, which Chromium shuts down cleanly on;
		// /F would terminate it mid-write.
		cmd = exec.Command("taskkill", "/IM", filepath.Base(b.Path))
	} else {
		cmd = exec.Command("pkill", "-TERM", "-f", b.Path)
	}
	cmd.Run() // a non-zero exit only means nothing matched
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := os.Open(cookieDB)
		if err == nil {
			f.Close()
			return nil
		}
		if os.IsNotExist(err) {
			return nil // nothing to wait for
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("%s still holds %s after %s; close it yourself and run this again", b.Name, cookieDB, timeout)
}

// cmdClone clones the user's real profile and launches the browser against
// the copy with flag-based remote debugging enabled.
//
//	use-browser clone [browser] [--profile <dir>] [--fresh] [--port <n>]
func cmdClone(args []string) error {
	name := ""
	profileDir := "Default"
	port := "" // empty: pick the first free port at launch time
	fresh := false
	noSync := false
	closeSource := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile", "-p":
			if i+1 < len(args) {
				i++
				profileDir = args[i]
			}
		case "--port":
			if i+1 < len(args) {
				i++
				port = args[i]
			}
		case "--fresh":
			fresh = true
		case "--no-sync":
			noSync = true
		case "--close-source":
			closeSource = true
		default:
			if !strings.HasPrefix(args[i], "-") && name == "" {
				name = args[i]
			}
		}
	}

	b, err := findBrowser(name)
	if err != nil {
		return err
	}
	pinBrowser(b.Name)
	// Decide before touching the remembered port. savePort here would
	// overwrite the port of a clone that is already running, and then
	// ownInstanceEndpoint could no longer find it.
	//
	// As in `launch`: only an instance we started counts. The real profile
	// behind the inspect toggle must not short-circuit the clone.
	// --close-source means "I want fresh logins", so attaching to the clone
	// that is already up would silently do nothing.
	if ownInstanceEndpoint(b.Name) != "" && !closeSource {
		fmt.Printf("ok already connected to %s (use-browser doctor for details)\n", b.Name)
		return nil
	}
	if d := cloneProfileDir(b.Name); profileInUse(d) && !closeSource {
		return fmt.Errorf("a %s is already using %s but is not serving DevTools.\nclose that browser and run this again", b.Name, d)
	}
	if port == "" {
		port = strconv.Itoa(freeDebugPort())
	}
	if n, err := strconv.Atoi(port); err == nil {
		savePort(n)
		writePortFile(cloneProfileDir(b.Name), n)
	}

	src := realUserDataDir(b.Name)
	if src == "" {
		return fmt.Errorf("don't know where %s stores its profile on this OS", b.Name)
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("no %s profile found at %s (has %s ever been run?)", b.Name, src, b.Name)
	}
	if _, err := os.Stat(filepath.Join(src, profileDir)); err != nil {
		avail := listProfiles(src)
		if len(avail) == 0 {
			return fmt.Errorf("profile %q not found in %s", profileDir, src)
		}
		return fmt.Errorf("profile %q not found; available in %s: %s\n(pick one with --profile \"Profile 1\")",
			profileDir, b.Name, strings.Join(avail, ", "))
	}

	clone := cloneProfileDir(b.Name)

	// The running browser holds its cookie database exclusively on Windows,
	// and that is the one file the logins live in. Closing it first is the
	// only way to sync logins without a human clicking anything.
	restoreSession := false
	if closeSource && b.isRunning() {
		fmt.Printf("closing %s so its cookies can be copied ...\n", b.Name)
		if err := closeBrowser(b, filepath.Join(src, profileDir, "Network", "Cookies"), 30*time.Second); err != nil {
			return err
		}
		// The clone runs the same executable, so that close request took it
		// down too. Wait for it to drop the singleton lock before writing.
		deadline := time.Now().Add(15 * time.Second)
		for profileInUse(clone) && time.Now().Before(deadline) {
			time.Sleep(300 * time.Millisecond)
		}
		if profileInUse(clone) {
			return fmt.Errorf("the cloned %s did not exit; close it and run this again", b.Name)
		}
		fmt.Printf("ok %s closed\n", b.Name)
		restoreSession = true
	}

	needCopy := fresh
	if _, err := os.Stat(filepath.Join(clone, profileDir)); err != nil {
		needCopy = true
	}
	if needCopy {
		if b.isRunning() {
			fmt.Printf("warning: %s is running; open cookie/login files may be locked and skipped.\n", b.Name)
			fmt.Println("         for a complete copy, close it first, then: use-browser clone " + b.Name + " --fresh")
		}
		if fresh {
			os.RemoveAll(clone)
		}
		fmt.Printf("cloning %s profile %q -> %s ...\n", b.Name, profileDir, clone)
		os.MkdirAll(clone, 0o755)
		// Local State holds the profile's encryption key; without it copied
		// cookies and saved passwords can't be decrypted.
		copyFile(filepath.Join(src, "Local State"), filepath.Join(clone, "Local State"))
		skipped := copyTree(filepath.Join(src, profileDir), filepath.Join(clone, profileDir))
		if skipped > 0 {
			fmt.Printf("copied with %d file(s) skipped (locked or unreadable)\n", skipped)
		}
	} else if noSync {
		fmt.Printf("reusing existing clone at %s (--no-sync given; drop it to pick up new logins)\n", clone)
	} else {
		// Refresh the copy so logins made in the real browser since the last
		// clone are carried over. Only changed files move, so this is cheap.
		// Ask the file, not the process list: a browser that has just been
		// asked to close still shows up in tasklist for a moment, and what
		// actually matters is whether this one file can be read. On Windows
		// it is opened without share-read, so it is exactly the file that
		// cannot be copied, and logins are exactly what the user wanted.
		if cookiesLocked(filepath.Join(src, profileDir, "Network", "Cookies")) {
			fmt.Printf("warning: %s holds its cookie database open, so logins will not sync.\n", b.Name)
			fmt.Printf("         everything else still syncs. for logins too:\n")
			fmt.Printf("           use-browser clone %s --close-source   (closes %s, reopens your tabs in the clone)\n", b.Name, b.Name)
		}
		fmt.Printf("syncing %s -> %s ...\n", filepath.Join(src, profileDir), filepath.Join(clone, profileDir))
		copyFile(filepath.Join(src, "Local State"), filepath.Join(clone, "Local State"))
		st := syncTree(filepath.Join(src, profileDir), filepath.Join(clone, profileDir))
		switch {
		case st.Copied == 0:
			fmt.Println("already up to date")
		default:
			fmt.Printf("synced %d file(s), %.1f MB", st.Copied, float64(st.Bytes)/(1024*1024))
			if st.Skipped > 0 {
				fmt.Printf(", %d skipped (locked or unreadable)", st.Skipped)
			}
			fmt.Println()
		}
	}

	cloneArgs := []string{
		"--remote-debugging-port=" + port,
		"--user-data-dir=" + clone,
		"--profile-directory=" + profileDir,
		"--no-first-run",
		"--no-default-browser-check",
	}
	// We just closed the browser the user was working in. Reopen their tabs
	// here, so the clone takes over rather than replacing what they had.
	if restoreSession {
		cloneArgs = append(cloneArgs, "--restore-last-session")
	}
	cmd := exec.Command(b.Path, cloneArgs...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %v", b.Path, err)
	}

	// As in `launch`: wait for our own clone, not for any endpoint the
	// inspect toggle may already be serving on the real profile.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ownInstanceEndpoint(b.Name) != "" {
			fmt.Printf("ok %s pid=%d profile=%q (cloned, real logins)\n", b.Name, cmd.Process.Pid, profileDir)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%s started (pid %d) but the DevTools endpoint never came up on :%s", b.Name, cmd.Process.Pid, port)
}
