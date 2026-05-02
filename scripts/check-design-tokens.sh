#!/usr/bin/env bash
# Fails if frontend/src/index.css redefines design-system tokens.
#
# The live frontend imports design-system/colors_and_type.css, which is the
# single source of truth for --color-*, --bg-*, --fg-*, --state-*,
# --accent-*, --border-*, --font-*, --text-*, --tracking-*, --leading-*,
# --radius-*, and --pulse-*. Redefining any of those in index.css means
# they will silently drift from the design system.
#
# Bare layout / component-internal vars (e.g. --sidebar-width) are fine to
# define in the live frontend — they're not part of the design vocabulary.

set -euo pipefail

cd "$(dirname "$0")/.."

LIVE=frontend/src/index.css
PROTECTED='--(color|bg|fg|surface|border|state|accent|font|text|tracking|leading|radius|pulse)[a-z0-9-]*:'

if matches=$(grep -nE "^[[:space:]]*${PROTECTED}" "${LIVE}" || true); [ -n "${matches}" ]; then
  echo "ERROR: ${LIVE} redefines design-system tokens."
  echo "These tokens belong in design-system/colors_and_type.css; remove them"
  echo "from ${LIVE} and let the @import provide them."
  echo
  echo "Offending lines:"
  echo "${matches}"
  exit 1
fi

echo "OK: ${LIVE} does not redefine design-system tokens."
