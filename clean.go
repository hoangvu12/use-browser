package main

// clean: show and remove the automation profiles use-browser has accumulated.
// A cloned profile is a copy of a real browser profile, so these directories
// get large (hundreds of MB each) and hold real cookies. They are worth being
// able to see and delete without hunting through the cache directory.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type profileEntry struct {
	Name    string // directory name, e.g. profile-chrome-clone
	Path    string
	Bytes   int64
	Cache   int64 // of Bytes, the part that is regenerable cache
	Running bool
}

func dirSize(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// walkCaches visits every cache directory in a profile tree. Chromium spreads
// them over several levels (User Data/Default/Cache, .../Service Worker), and
// they are the part of a clone that grows without ever being copied.
func walkCaches(path string, fn func(dir string)) {
	filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if cacheDirs[d.Name()] {
			fn(p)
			return filepath.SkipDir
		}
		return nil
	})
}

func cacheSize(path string) int64 {
	var total int64
	walkCaches(path, func(dir string) { total += dirSize(dir) })
	return total
}

// dropCaches deletes a profile's cache directories and leaves everything that
// carries identity -- cookies, logins, history -- untouched. The browser
// rebuilds what it needs on the next run, more slowly the first time.
func dropCaches(path string) (freed int64) {
	var dirs []string
	walkCaches(path, func(dir string) { dirs = append(dirs, dir) })
	for _, dir := range dirs {
		size := dirSize(dir)
		if os.RemoveAll(dir) == nil {
			freed += size
		}
	}
	return freed
}

// listAutomationProfiles finds every profile-* directory under the state dir,
// regardless of session, because clean is about disk, not about routing.
func listAutomationProfiles() []profileEntry {
	dir := stateDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []profileEntry
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "profile-") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		// Ask the singleton lock as well as the port: Chrome and Brave often
		// never write DevToolsActivePort, and deleting a profile that is in
		// use corrupts it.
		port, _ := activePort(p)
		out = append(out, profileEntry{
			Name:    e.Name(),
			Path:    p,
			Bytes:   dirSize(p),
			Cache:   cacheSize(p),
			Running: profileInUse(p) || (port != "" && portAlive(port)),
		})
	}
	return out
}

func mb(b int64) float64 { return float64(b) / (1024 * 1024) }

// cmdClean lists the automation profiles, or deletes the named ones.
//
//	use-browser clean                 list them with sizes
//	use-browser clean profile-chrome  delete one
//	use-browser clean --all           delete every one that is not running
//	use-browser clean --cache         delete only the caches, keep the logins
func cmdClean(args []string) error {
	profiles := listAutomationProfiles()
	if len(profiles) == 0 {
		fmt.Printf("no automation profiles in %s\n", stateDir())
		return nil
	}
	all, cacheOnly := false, false
	var names []string
	for _, a := range args {
		switch a {
		case "--all":
			all = true
			continue
		case "--cache":
			cacheOnly = true
			continue
		}
		names = append(names, strings.TrimSuffix(a, string(filepath.Separator)))
	}

	if !all && !cacheOnly && len(names) == 0 {
		var total, cache int64
		fmt.Printf("automation profiles in %s:\n", stateDir())
		for _, p := range profiles {
			note := ""
			if p.Cache > 0 {
				note = fmt.Sprintf("   %.0f MB cache", mb(p.Cache))
			}
			if p.Running {
				note += "  [running]"
			}
			fmt.Printf("  %-28s %8.1f MB%s\n", p.Name, mb(p.Bytes), note)
			total += p.Bytes
			cache += p.Cache
		}
		fmt.Printf("  %-28s %8.1f MB\n", "total", mb(total))
		fmt.Println("delete with: use-browser clean <name>...   |   use-browser clean --all")
		if cache > 0 {
			fmt.Printf("keep the logins, drop %.0f MB of cache: use-browser clean --cache\n", mb(cache))
		}
		fmt.Println("a deleted clone is re-copied on the next: use-browser clone <browser>")
		return nil
	}

	byName := map[string]profileEntry{}
	for _, p := range profiles {
		byName[p.Name] = p
	}
	var targets []profileEntry
	if all || (cacheOnly && len(names) == 0) {
		targets = profiles
	} else {
		for _, n := range names {
			p, ok := byName[n]
			if !ok {
				return fmt.Errorf("no automation profile named %q (run: use-browser clean)", n)
			}
			targets = append(targets, p)
		}
	}

	var freed int64
	for _, p := range targets {
		// Deleting a profile out from under a live browser corrupts it and
		// leaves the browser writing into nothing.
		if p.Running {
			fmt.Printf("skipped %s: a browser is using it (close it first)\n", p.Name)
			continue
		}
		if cacheOnly {
			got := dropCaches(p.Path)
			fmt.Printf("cleared cache in %s (%.1f MB, logins kept)\n", p.Name, mb(got))
			freed += got
			continue
		}
		if err := os.RemoveAll(p.Path); err != nil {
			fmt.Printf("skipped %s: %v\n", p.Name, err)
			continue
		}
		fmt.Printf("removed %s (%.1f MB)\n", p.Name, mb(p.Bytes))
		freed += p.Bytes
	}
	fmt.Printf("freed %.1f MB\n", mb(freed))
	return nil
}
