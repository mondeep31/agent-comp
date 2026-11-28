#!/usr/bin/env bash
# Build the headless "base" image. Kept for existing scripts; see build-image.sh.
#
# Usage: sudo images/build-base.sh [OUT_DIR]
exec "$(dirname "$0")/build-image.sh" base "$@"
