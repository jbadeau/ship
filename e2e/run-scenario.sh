#!/usr/bin/env bash
# Runs scenario.sh inside the sandbox network with the ship:dev image.
set -euo pipefail
cd "$(dirname "$0")"
exec docker run --rm --network ship-e2e --env-file .state.env -v "$PWD:/e2e:ro" ship:dev bash /e2e/scenario.sh
