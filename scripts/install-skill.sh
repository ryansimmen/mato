#!/bin/sh
# Install mato skill to ~/.copilot/skills/mato/ unconditionally, and prompt
# before installing to ~/.claude/skills/mato/ (Claude Code) and
# ~/.config/opencode/skills/mato/ (OpenCode) when those CLIs are detected.
#
# Prompt behavior can be overridden with:
#   --yes                       install to all detected targets without prompting
#   --no                        skip Claude/OpenCode without prompting
#   MATO_SKILL_INSTALL=yes|no   same as --yes / --no
#
# When stdin is not a TTY and no override is given, Claude/OpenCode targets
# are skipped with an informational message.

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SOURCE_DIR="$SCRIPT_DIR/../.github/skills/mato"

assume=""
case "${MATO_SKILL_INSTALL:-}" in
  yes|YES|y|Y) assume="yes" ;;
  no|NO|n|N) assume="no" ;;
  "") ;;
  *)
    echo "error: MATO_SKILL_INSTALL must be 'yes' or 'no'" >&2
    exit 2
    ;;
esac

usage() {
  cat <<'EOF'
Usage: install-skill.sh [--yes|--no]

Installs the bundled mato skill into local CLI skill directories.

Options:
  --yes      Install to all detected targets without prompting
  --no       Skip Claude/OpenCode without prompting
  -h, --help Show this help

Environment:
  MATO_SKILL_INSTALL=yes|no   Same as --yes / --no
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --yes) assume="yes" ;;
    --no) assume="no" ;;
    -h|--help) usage; exit 0 ;;
    *)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

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

confirm_install() {
  name="$1"
  target_dir="$2"

  case "$assume" in
    yes)
      install_skill "$target_dir"
      return
      ;;
    no)
      echo "Skipping $name skill install ($target_dir)"
      return
      ;;
  esac

  if [ ! -t 0 ]; then
    echo "Skipping $name skill install ($target_dir): non-interactive; pass --yes or set MATO_SKILL_INSTALL=yes to auto-install"
    return
  fi

  printf 'Install mato skill to %s? [y/N] ' "$target_dir"
  if ! IFS= read -r reply; then
    echo
    echo "Skipping $name skill install ($target_dir)"
    return
  fi
  case "$reply" in
    y|Y|yes|YES|Yes) install_skill "$target_dir" ;;
    *) echo "Skipping $name skill install ($target_dir)" ;;
  esac
}

install_skill "$HOME/.copilot/skills/mato"

if command -v claude >/dev/null 2>&1; then
  confirm_install "Claude Code" "$HOME/.claude/skills/mato"
fi

if command -v opencode >/dev/null 2>&1; then
  confirm_install "OpenCode" "$HOME/.config/opencode/skills/mato"
fi
