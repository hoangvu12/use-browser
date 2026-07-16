package main

import _ "embed"

// Agent-facing skill text. SKILL.md is the single source of truth; the
// binary embeds it so `use-browser skill` always matches the repo file.
// Save it with: use-browser skill > .claude/skills/use-browser/SKILL.md
//
//go:embed SKILL.md
var skillText string
