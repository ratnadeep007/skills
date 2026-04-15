# Agent Skills

A collection of reusable AI agent skills that work across Claude Code, Cursor, Windsurf, Codex, and other coding agents that follow the `SKILL.md` convention.

## What is a Skill?

A skill is a directory containing a `SKILL.md` file that teaches an AI agent how to perform a specialized task. When you invoke a skill, the agent reads `SKILL.md` and follows the instructions inside — no plugins, no extensions, just a file the agent reads.

```
python-code-reviewer/
├── SKILL.md          ← instructions the agent reads and follows
├── scripts/          ← helper scripts (optional)
│   ├── analyze_diff.py
│   └── trace_callers.py
└── references/       ← reference material the skill uses
    └── breakage_patterns.md
```

## Skills in this Repo

| Skill | Description |
|-------|-------------|
| [`python-code-reviewer`](./python-code-reviewer/) | Deep Python breakage analysis — traces call graphs, signatures, and import trees across a diff to surface runtime risks before merge |

---

## Installation

Skills need to be placed in the directory your agent reads from. Each agent has a different location.

### Claude Code

```bash
# Clone the repo
git clone https://github.com/<your-username>/skills.git ~/skills-repo

# Copy the skill to Claude's skills directory
cp -r ~/skills-repo/python-code-reviewer ~/.claude/skills/python-code-review
```

Claude Code reads skills from `~/.claude/skills/<skill-name>/SKILL.md`.

To install all skills at once:

```bash
for skill in ~/skills-repo/*/; do
  name=$(basename "$skill")
  cp -r "$skill" ~/.claude/skills/"$name"
done
```

### Cursor

```bash
# Copy to Cursor's skills directory
cp -r ~/skills-repo/python-code-reviewer ~/.cursor/skills-cursor/python-code-review
```

Cursor reads skills from `~/.cursor/skills-cursor/<skill-name>/SKILL.md`.

### Windsurf

```bash
cp -r ~/skills-repo/python-code-reviewer ~/.windsurf/skills/python-code-review
```

### OpenCode / Codex CLI

```bash
cp -r ~/skills-repo/python-code-reviewer ~/.opencode/skills/python-code-review
```

### Gemini CLI

```bash
cp -r ~/skills-repo/python-code-reviewer ~/.gemini/skills/python-code-review
```

> **Note:** Skill directory paths vary by agent version. If the path above doesn't work, check your agent's documentation for where it reads custom skills or prompts.

---

## Staying Up to Date

To update a skill to the latest version from this repo:

```bash
# Pull latest changes
cd ~/skills-repo && git pull

# Re-copy updated skill (overwrites previous)
cp -r ~/skills-repo/python-code-reviewer ~/.claude/skills/python-code-review
```

Or keep the repo as the source of truth by symlinking instead of copying:

```bash
ln -sf ~/skills-repo/python-code-reviewer ~/.claude/skills/python-code-review
```

With a symlink, `git pull` in the repo automatically updates the live skill — no re-copy needed.

---

## Using a Skill

Once installed, invoke a skill by name in your agent's chat:

```
/python-code-review
```

Or describe what you want and the agent will pick it up automatically:

```
review my changes for anything that could break
```

Agents that support skill auto-detection (Claude Code, Cursor) will match the skill's trigger phrases defined in `SKILL.md` and invoke it without an explicit slash command.

### Passing arguments

Most skills accept arguments after the slash command:

```
/python-code-review --base origin/main
/python-code-review --file /tmp/my.diff
/python-code-review --help
```

---

## Writing Your Own Skill

A minimal skill is just a `SKILL.md` with a frontmatter block and instructions:

```markdown
---
name: my-skill
description: >
  One-paragraph description. Include trigger phrases so agents know
  when to auto-invoke this skill.
---

# My Skill

Instructions for the agent go here. Be specific — the agent follows
these instructions literally when the skill is invoked.
```

Place it in `~/.claude/skills/my-skill/SKILL.md` and invoke it with `/my-skill`.

See the [create-skill](https://github.com/buildbetter-app/bb-skills) skill for guided authoring assistance.

---

## License

MIT
