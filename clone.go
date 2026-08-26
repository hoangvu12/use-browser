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
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
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
	if port == "" {
		port = strconv.Itoa(freeDebugPort())
	}
	if n, err := strconv.Atoi(port); err == nil {
		savePort(n)
	}
	if connected() {
		fmt.Printf("ok already connected to %s (use-browser doctor for details)\n", b.Name)
		return nil
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

	clone := filepath.Join(filepath.Dir(stateFile()), "profile-"+b.Name+"-clone")

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
	} else {
		fmt.Printf("reusing existing clone at %s (--fresh to re-copy from your real profile)\n", clone)
	}

	cmd := exec.Command(b.Path,
		"--remote-debugging-port="+port,
		"--user-data-dir="+clone,
		"--profile-directory="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %v", b.Path, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if connected() {
			fmt.Printf("ok %s pid=%d profile=%q (cloned, real logins)\n", b.Name, cmd.Process.Pid, profileDir)
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("%s started (pid %d) but the DevTools endpoint never came up on :%s", b.Name, cmd.Process.Pid, port)
}
