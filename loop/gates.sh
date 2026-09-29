#!/bin/bash
# Default: quick local checks. --candidate adds safety sentinels; --audit requests all mutations.
# Local checks only; no mode authorizes live use or conductor advancement.
set -euo pipefail
cd "$(dirname "$0")/.."
exec /Users/hugh/kek/.venv/bin/python scripts/run_gates.py "$@"
