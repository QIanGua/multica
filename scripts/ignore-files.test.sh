#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
failures=0

fail() {
  echo "ERROR: $*" >&2
  failures=$((failures + 1))
}

require_rule() {
  local ignore_file=$1
  local rule=$2
  if ! grep -Fqx -- "$rule" "$repo_root/$ignore_file"; then
    fail "$ignore_file must contain $rule"
  fi
}

forbid_rule() {
  local ignore_file=$1
  local rule=$2
  if grep -Fqx -- "$rule" "$repo_root/$ignore_file"; then
    fail "$ignore_file contains stale rule $rule"
  fi
}

check_tracked_root_rules() {
  local ignore_file=$1
  local rule candidate

  while IFS= read -r rule || [ -n "$rule" ]; do
    case "$rule" in
      /*) ;;
      *) continue ;;
    esac

    candidate=${rule#/}
    candidate=${candidate%/}

    # Globs describe a class of files, not one repository path. Validate only
    # literal root-anchored rules; local/generated patterns remain intentionally
    # outside this check.
    if printf '%s\n' "$candidate" | grep -q '[][?*]'; then
      continue
    fi

    if [ -z "$(git -C "$repo_root" ls-files -- "$candidate")" ]; then
      fail "$ignore_file points at untracked or missing root path: /$candidate"
    fi
  done < "$repo_root/$ignore_file"
}

require_rule .dockerignore /apps/docs/
require_rule .dockerignore /apps/mobile/
require_rule .dockerignore /.github/
require_rule .dockerignore /deploy/
require_rule .dockerignore /scripts/
require_rule .dockerignore '*.md'

require_rule .vercelignore /apps/mobile/
require_rule .vercelignore /README.zh.md
forbid_rule .vercelignore /HANDOFF_ARCHITECTURE_AUDIT.md
forbid_rule .vercelignore /README.zh-CN.md
forbid_rule .vercelignore .husky

check_tracked_root_rules .dockerignore
check_tracked_root_rules .vercelignore

if [ "$failures" -ne 0 ]; then
  exit 1
fi

echo "Ignore-file invariants are valid."
