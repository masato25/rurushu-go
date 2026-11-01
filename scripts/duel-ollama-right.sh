#!/usr/bin/env bash
set -Eeuo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
RURUSHU_BIN="${RURUSHU_BIN:-${ROOT}/bin/rurushu}"
OLLAMA_URL="${RURUSHU_OLLAMA_URL:-http://192.168.2.109:11434}"
RIGHT_MODEL="${RURUSHU_RIGHT_MODEL:-hf.co/unsloth/Qwen3.8-27B-GGUF:UD-IQ2_M}"

usage() {
  cat <<EOF
Usage:
  ./scripts/duel-ollama-right.sh --new --goal "<goal>"
  ./scripts/duel-ollama-right.sh --resume

Starts duel with:
  LEFT   provider=cos model=chatgpt-web
  RIGHT  provider=ollama model=${RIGHT_MODEL}
  Ollama ${OLLAMA_URL}

All arguments are forwarded to 'rurushu duel'.

Optional environment overrides:
  RURUSHU_BIN=/path/to/rurushu
  RURUSHU_OLLAMA_URL=http://host:11434
  RURUSHU_RIGHT_MODEL=model-name

Example:
  ./scripts/duel-ollama-right.sh --new \
    --goal "繼續開發目前功能"
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ ! -x "$RURUSHU_BIN" ]]; then
  echo "rurushu binary not found: $RURUSHU_BIN" >&2
  echo "Run ./build.sh first, or set RURUSHU_BIN." >&2
  exit 1
fi

mode=""
for arg in "$@"; do
  case "$arg" in
    --new) mode="new" ;;
    --resume) mode="resume" ;;
  esac
done

if [[ -z "$mode" ]]; then
  echo "Refusing to reuse an existing duel implicitly." >&2
  echo "Pass --new for a fresh Ollama RIGHT duel, or --resume for one already created with this launcher." >&2
  echo >&2
  usage >&2
  exit 2
fi

if [[ "$mode" == "resume" ]]; then
  STATE_FILE="${ROOT}/.rurushu/duel-state.json"
  if [[ ! -f "$STATE_FILE" ]]; then
    echo "Cannot resume: no duel state at $STATE_FILE" >&2
    exit 1
  fi
  if ! grep -Eq '"right_provider"[[:space:]]*:[[:space:]]*"ollama"' "$STATE_FILE"; then
    echo "Cannot resume with Ollama launcher: existing duel was not created with RIGHT provider=ollama." >&2
    echo "Start a fresh test with: ./scripts/duel-ollama-right.sh --new --goal \"<goal>\"" >&2
    exit 2
  fi
fi

exec "$RURUSHU_BIN" duel \
  --left-provider cos \
  --left-model chatgpt-web \
  --right-provider ollama \
  --right-model "$RIGHT_MODEL" \
  --ollama-url "$OLLAMA_URL" \
  "$@"
