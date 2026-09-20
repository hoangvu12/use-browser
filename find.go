package main

// find: element picking by description, via Jev (TypeSafe System One).
//
// snap prints every interactive element into the agent's context and the
// agent picks an index; on an element-heavy page that round trip is the most
// expensive part of the task. find skips it: the CLI snaps the page
// internally (parking window.__bu, so the index it reports is a real snap
// index that click/fill resolve), sends the indexed lines to Jev in one
// request, and prints the pick with the numbers to judge it by:
//
//	ok [7]<button> "Sign in"  p=0.92 conf=0.85 exists=0.97
//
// One request carries two questions, the pattern from TypeSafe's
// semantic-find cookbook: a Choice over the indexes (plus a "none" option)
// points at the element, and a Noul checks that any element matches at all —
// Choice probabilities always sum to 1, so a line always "wins" even when
// nothing matches.
//
// Jev is a decision model, not a trusted one: page content is untrusted
// input to it, and a deceptive label can steer the pick. find only acts
// (--click/--fill) when the top probability and the existence check are both
// >= 0.5; anything else fails the way a stale index does — candidates
// printed, plus the `run: use-browser snap` way out.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	tsEndpoint = "https://api.typesafe.ai/v1/systemone"
	tsModel    = "jev-latest"
	tsTimeout  = 60 * time.Second

	findDefaultN = 150 // snap's default; a small state keeps Jev accurate
	findMaxN     = 254 // Choice allows at most 255 options; "none" takes one
)

// cmdFind: `find "<description>" [--click | --fill <text>] [--max N]`
func cmdFind(c *cdpClient, args []string) error {
	var doClick, doFill bool
	var fillText string
	n := findDefaultN
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--click":
			doClick = true
		case "--fill":
			if i+1 >= len(args) {
				return fmt.Errorf("--fill needs a value")
			}
			i++
			fillText = args[i]
			doFill = true
		case "--max":
			if i+1 >= len(args) {
				return fmt.Errorf("--max needs a value")
			}
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				return fmt.Errorf("--max: %q is not a positive number", args[i+1])
			}
			i++
			n = v
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 {
		return fmt.Errorf(`usage: use-browser find "<description>" [--click | --fill <text>] [--max N]`)
	}
	if doClick && doFill {
		return fmt.Errorf("use --click or --fill, not both")
	}
	key := apiKey()
	if key == "" {
		return fmt.Errorf("find needs a TypeSafe API key; set one up with: use-browser apikey set <key> (from https://console.typesafe.ai)")
	}
	desc := strings.Join(rest, " ")

	// Internal snapshot: the refs it parks make the reported index clickable.
	raw, err := c.evalString(snapJS)
	if err != nil {
		return err
	}
	var snap snapResult
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return fmt.Errorf("bad snapshot: %v", err)
	}
	if snap.Total == 0 {
		return fmt.Errorf("no interactive elements on this page")
	}
	lines := snap.Lines
	if n > findMaxN {
		n = findMaxN
	}
	if len(lines) > n {
		lines = lines[:n]
	}

	// State: the tagged element lines. The [N] prefix doubles as the option
	// key, so the answer needs no mapping.
	var state strings.Builder
	fmt.Fprintf(&state, "Page %s %q\n\n", snap.URL, snap.Title)
	for _, l := range lines {
		state.WriteString(l)
		state.WriteByte('\n')
	}

	d := strings.ReplaceAll(desc, `"`, `'`)
	criteria := make(map[string]any, len(lines)+1)
	for i := range lines {
		criteria[strconv.Itoa(i+1)] = nil
	}
	criteria["none"] = "no element in the snapshot matches the description"

	res, err := tsCall(key, tsRequest{
		State: state.String(),
		Model: tsModel,
		Questions: map[string]tsQuestion{
			"where": {
				Type:         "choice",
				Instructions: fmt.Sprintf("Which numbered element in the snapshot matches the description: %q?", d),
				Criteria:     criteria,
			},
			"exists": {
				Type:         "noul",
				Instructions: fmt.Sprintf("Does any element in the snapshot match the description: %q?", d),
				Criteria: map[string]any{
					"true":  "At least one element in the snapshot matches the description",
					"false": "No element in the snapshot matches the description",
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("typesafe: %v", err)
	}

	where := res.Answers.Where
	exists := res.Answers.Exists.Noul
	idx, idxErr := strconv.Atoi(where.Choice)
	if idxErr == nil && idx >= 1 && idx <= len(lines) &&
		where.Probabilities[where.Choice] >= 0.5 && exists >= 0.5 {
		fmt.Printf("ok %s  p=%.2f conf=%.2f exists=%.2f\n",
			lines[idx-1], where.Probabilities[where.Choice], where.Confidence, exists)
		if doClick {
			return cmdClick(c, []string{strconv.Itoa(idx)})
		}
		if doFill {
			return cmdFill(c, []string{strconv.Itoa(idx), fillText})
		}
		return nil
	}

	// No confident pick: show the ranking, then fail like a stale index.
	type cand struct {
		line string
		p    float64
	}
	var cs []cand
	for k, p := range where.Probabilities {
		i, err := strconv.Atoi(k)
		if err != nil || i < 1 || i > len(lines) {
			continue
		}
		cs = append(cs, cand{lines[i-1], p})
	}
	sort.Slice(cs, func(a, b int) bool { return cs[a].p > cs[b].p })
	if len(cs) > 3 {
		cs = cs[:3]
	}
	if len(cs) > 0 {
		fmt.Println("candidates:")
		for _, cd := range cs {
			fmt.Printf("%s  p=%.2f\n", cd.line, cd.p)
		}
	}
	hint := "no confident match; run: use-browser snap and pick an index yourself"
	if snap.Total > len(lines) {
		hint = fmt.Sprintf("no confident match (searched %d of %d elements); run: use-browser snap", len(lines), snap.Total)
	}
	return fmt.Errorf("find: %s (exists=%.2f)", hint, exists)
}

type tsQuestion struct {
	Type         string         `json:"type"`
	Instructions string         `json:"instructions"`
	Criteria     map[string]any `json:"criteria,omitempty"`
}

type tsRequest struct {
	State     string                `json:"state"`
	Model     string                `json:"model"`
	Questions map[string]tsQuestion `json:"questions"`
}

type tsResponse struct {
	Answers struct {
		Where struct {
			Choice        string             `json:"choice"`
			Probabilities map[string]float64 `json:"probabilities"`
			Confidence    float64            `json:"confidence"`
		} `json:"where"`
		Exists struct {
			Noul float64 `json:"noul"`
		} `json:"exists"`
	} `json:"answers"`
}

// tsCall posts one System One request and parses the typed response.
func tsCall(key string, req tsRequest) (*tsResponse, error) {
	b, err := tsCallRaw(key, req)
	if err != nil {
		return nil, err
	}
	var out tsResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("bad response: %v", err)
	}
	return &out, nil
}

// tsCallRaw posts one System One request and returns the raw response body,
// for callers (run) that need answers under their own keys. Rate limits
// (429/529) and transport errors get a single retry; the API is small enough
// that nothing fancier is warranted.
func tsCallRaw(key string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: tsTimeout}
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			time.Sleep(1500 * time.Millisecond)
		}
		r, err := http.NewRequest(http.MethodPost, tsEndpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(r)
		if err != nil {
			if attempt == 0 {
				continue
			}
			return nil, err
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return b, nil
		}
		if (resp.StatusCode == 429 || resp.StatusCode == 529) && attempt == 0 {
			continue
		}
		msg := string(b)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
}
