package main

// key: storage for the TypeSafe API key that powers find. An environment
// variable is a poor home for a secret a CLI needs across sessions and
// shells, so `use-browser key set` writes it to a small file in the state
// dir (0600) and `key show` prints only a masked form — the full value never
// lands in a transcript. TYPESAFE_API_KEY still wins when set, so CI and
// one-off overrides keep working; the file is the normal path.
//
// A configured key is also the opt-in signal: agents should reach for find
// when one exists and stick to snap when one doesn't.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func keyFile() string { return filepath.Join(stateDir(), "typesafe-key") }

// apiKey returns the key find should use: the env var when set, else the
// stored one. Empty means find is unavailable.
func apiKey() string {
	if k := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")); k != "" {
		return k
	}
	b, err := os.ReadFile(keyFile())
	if err != nil {
		return ""
	}
	// tolerate a BOM from hand-editing with PowerShell's Set-Content
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	return strings.TrimSpace(string(b))
}

// keySource reports where the active key comes from: "env", "file" or "".
func keySource() string {
	if strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")) != "" {
		return "env"
	}
	b, err := os.ReadFile(keyFile())
	if err == nil && strings.TrimSpace(string(b)) != "" {
		return "file"
	}
	return ""
}

func maskKey(k string) string {
	if len(k) <= 16 {
		return strings.Repeat("*", len(k))
	}
	return k[:10] + "..." + k[len(k)-6:]
}

// cmdAPIKey: `apikey set <value>` (or `apikey set -` to read stdin, keeping
// the key out of shell history) | `apikey show` (masked) | `apikey clear`.
// Bare `apikey` reports status, like `use` with no arguments. Named apikey,
// not key, because `key Enter` already sends a keypress.
func cmdAPIKey(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch {
	case len(args) == 2 && args[0] == "set":
		k := args[1]
		if k == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return err
			}
			k = string(bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF}))
		}
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " \t\r\n") {
			return fmt.Errorf("that does not look like an API key")
		}
		if err := os.MkdirAll(stateDir(), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(keyFile(), []byte(k), 0o600); err != nil {
			return err
		}
		fmt.Printf("ok key stored %s (use-browser find is now Jev-powered; the key is verified on first use)\n", maskKey(k))
		return nil
	case len(args) == 1 && args[0] == "show":
		if k := apiKey(); k != "" {
			fmt.Printf("%s (source: %s)\n", maskKey(k), keySource())
		} else {
			fmt.Println("no key; find falls back to snap + click. Set one with:")
			fmt.Println("  use-browser apikey set <key>   (from https://console.typesafe.ai)")
		}
		return nil
	case len(args) == 1 && args[0] == "clear":
		if err := os.Remove(keyFile()); err != nil && !os.IsNotExist(err) {
			return err
		}
		if keySource() == "env" {
			fmt.Println("ok stored key cleared (TYPESAFE_API_KEY is still set and still wins)")
		} else {
			fmt.Println("ok key cleared; find falls back to snap + click")
		}
		return nil
	default:
		return fmt.Errorf("usage: use-browser apikey set <key> | show | clear   (apikey set - reads the key from stdin)")
	}
}
