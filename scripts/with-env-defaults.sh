#!/bin/sh
# Runs a command with every variable of .env.example that is not already set
# in the environment. Values already exported win, so overrides keep working.
set -eu
env_file="${ENV_FILE:-.env.example}"
while IFS= read -r line || [ -n "$line" ]; do
	case "$line" in '' | '#'*) continue ;; esac
	key="${line%%=*}"
	if ! printenv "$key" >/dev/null 2>&1; then
		export "$key=${line#*=}"
	fi
done <"$env_file"
exec "$@"
