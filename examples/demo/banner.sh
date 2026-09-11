#!/usr/bin/env bash
# Print the demo GIF's colored section banners. One place for all banner
# wording so the tape stays clean: slides just run "./banner.sh <n>".
# Usage: banner.sh <1-5|ok>
set -euo pipefail

case "${1:-}" in
1)
	printf '\n\033[1;44m 1 · YOUR REQUEST \033[0m  fake secrets inside — nothing has left your machine\n'
	;;
2)
	printf '\n\033[1;46m 2 · THROUGH NENYA \033[0m  streaming reply\n'
	;;
3)
	printf '\n\033[1;42m 3 · WHAT THE PROVIDER RECEIVED \033[0m  secrets stripped\n'
	;;
4)
	printf '\n\033[1;43m 4 · PROOF \033[0m\n'
	;;
5)
	printf '\n\033[1;43m 5 · EFFICIENCY \033[0m  same request again → cache hit; client vs upstream tokens\n'
	;;
ok)
	printf '\n\033[1;32m✔ redacted · trimmed · cached — the upstream saw the minimum\033[0m\n\n'
	;;
*)
	echo "usage: banner.sh <1-5|ok>" >&2
	exit 1
	;;
esac
