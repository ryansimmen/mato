#!/bin/sh
# Install mato-skill to ~/.copilot/skills/mato-skill/
# Usage: ./install.sh

set -e

SKILL_DIR="$HOME/.copilot/skills/mato-skill"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SOURCE_DIR="$SCRIPT_DIR/.github/skills/mato-skill"

if [ ! -f "$SOURCE_DIR/SKILL.md" ]; then
  echo "error: SKILL.md not found at $SOURCE_DIR" >&2
  exit 1
fi

mkdir -p "$SKILL_DIR"
cp "$SOURCE_DIR"/SKILL.md "$SKILL_DIR/"

echo "Installed mato-skill to $SKILL_DIR"
