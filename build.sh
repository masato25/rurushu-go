#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="${ROOT}/bin/rurushu"
RUN_TESTS=1

usage() {
  cat <<'HELP'
Usage: ./build.sh [--skip-tests] [--output PATH] [--help]

Build rurushu from any working directory.
  --skip-tests    Skip go test -count=1 ./...
  --output PATH   Set binary destination (default: <repo>/bin/rurushu)
  --help          Show this help
HELP
}

while (($#)); do
  case "$1" in
    --skip-tests) RUN_TESTS=0; shift ;;
    --output)
      if (($# < 2)) || [[ -z "$2" ]]; then echo 'Missing --output path' >&2; exit 2; fi
      OUTPUT="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

command -v go >/dev/null || { echo 'Go is not installed or not in PATH' >&2; exit 1; }
cd "$ROOT"
if ((RUN_TESTS)); then
  echo '==> Running Go tests'
  go test -count=1 ./...
fi

mkdir -p -- "$(dirname -- "$OUTPUT")"
TEMP="$(mktemp "$(dirname -- "$OUTPUT")/.rurushu-build.XXXXXXXX")"
trap 'rm -f -- "$TEMP"' EXIT

echo '==> Building rurushu'
go build -trimpath -o "$TEMP" ./cmd/rurushu
chmod 755 "$TEMP"
mv -f -- "$TEMP" "$OUTPUT"
echo "==> Built: $OUTPUT"
"$OUTPUT" duel --help >/dev/null 2>&1
echo '==> Smoke test passed'
