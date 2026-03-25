#!/bin/sh
# Install mato skill to ~/.copilot/skills/mato/ and, when OpenCode is
# available, ~/.config/opencode/skills/mato/.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SOURCE_DIR="$SCRIPT_DIR/../.github/skills/mato"

if [ ! -f "$SOURCE_DIR/SKILL.md" ]; then
  echo "error: SKILL.md not found at $SOURCE_DIR" >&2
  exit 1
fi

install_skill() {
  target_dir="$1"
  mkdir -p "$target_dir"
  cp "$SOURCE_DIR"/SKILL.md "$target_dir/"
  echo "Installed mato skill to $target_dir"
}

install_skill "$HOME/.copilot/skills/mato"

if command -v opencode >/dev/null 2>&1; then
  install_skill "$HOME/.config/opencode/skills/mato"
fi
