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
		port, _ := activePort(p)
		out = append(out, profileEntry{
			Name:    e.Name(),
			Path:    p,
			Bytes:   dirSize(p),
			Running: port != "" && portAlive(port),
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
func cmdClean(args []string) error {
	profiles := listAutomationProfiles()
	if len(profiles) == 0 {
		fmt.Printf("no automation profiles in %s\n", stateDir())
		return nil
	}
	all := false
	var names []string
	for _, a := range args {
		if a == "--all" {
			all = true
			continue
		}
		names = append(names, strings.TrimSuffix(a, string(filepath.Separator)))
	}

	if !all && len(names) == 0 {
		var total int64
		fmt.Printf("automation profiles in %s:\n", stateDir())
		for _, p := range profiles {
			mark := ""
			if p.Running {
				mark = "  [running]"
			}
			fmt.Printf("  %-28s %8.1f MB%s\n", p.Name, mb(p.Bytes), mark)
			total += p.Bytes
		}
		fmt.Printf("  %-28s %8.1f MB\n", "total", mb(total))
		fmt.Println("delete with: use-browser clean <name>...   |   use-browser clean --all")
		fmt.Println("a deleted clone is re-copied on the next: use-browser clone <browser>")
		return nil
	}

	byName := map[string]profileEntry{}
	for _, p := range profiles {
		byName[p.Name] = p
	}
	var targets []profileEntry
	if all {
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
