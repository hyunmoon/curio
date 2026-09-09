#!/usr/bin/env bash
# Personal-only build; does not set service environment or install a binary.
exec python3 "$(dirname "$0")/pc1_build.py" build --profile sdisk-sm "$@"
