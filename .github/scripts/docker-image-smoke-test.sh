#!/usr/bin/env bash
#
# docker-image-smoke-test.sh -- check a locally loaded ccv2-geth image before it
# is published to Docker Hub.
#
#   .github/scripts/docker-image-smoke-test.sh <image> <git-tag>
#
# Run from the repository root, with the <git-tag> commit checked out:
#
#   1. The tag is vX.Y.Z or vX.Y.Z-rcN, and X.Y.Z equals the compiled-in version
#      in both version/version.go and params/version.go (CLAUDE.md: the release
#      tag must always equal the compiled-in version).
#   2. `geth version` inside the image reports that same X.Y.Z.
#   3. For each live network (--spicy, --chiliz) geth starts offline and the fork
#      schedule it loads (admin.nodeInfo.protocols.eth.config) carries every
#      `*Time` key of config/embedded/<network>.json with the same value, plus
#      the same chainId. It is the check that was run by hand on v2.10.9-rc1
#      before that image was pushed.
#
# Exits non-zero on the first mismatch. Never pushes anything.

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <image> <git-tag>" >&2
  exit 2
fi
IMAGE="$1"
TAG="$2"

fail() { echo "::error::$*" >&2; exit 1; }

# Containers started by check_network, removed on any exit (including fail).
CONTAINERS=()
cleanup() {
  local c
  for c in "${CONTAINERS[@]+"${CONTAINERS[@]}"}"; do
    docker rm -f "$c" >/dev/null 2>&1 || echo "warning: could not remove container $c" >&2
  done
}
trap cleanup EXIT

# --- 1. tag format and compiled-in version ---------------------------------
if [[ ! "$TAG" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)(-rc[0-9]+)?$ ]]; then
  fail "tag '$TAG' is not vX.Y.Z or vX.Y.Z-rcN"
fi
WANT="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.${BASH_REMATCH[3]}"

read_version() { # <file> <prefix>: prints MAJOR.MINOR.PATCH from the Go constants
  local file="$1" prefix="$2" major minor patch
  major=$(sed -nE "s/^[[:space:]]*${prefix}Major[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p" "$file")
  minor=$(sed -nE "s/^[[:space:]]*${prefix}Minor[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p" "$file")
  patch=$(sed -nE "s/^[[:space:]]*${prefix}Patch[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p" "$file")
  [[ -n "$major" && -n "$minor" && -n "$patch" ]] || fail "could not read the version constants from $file"
  echo "${major}.${minor}.${patch}"
}
V_VERSION=$(read_version version/version.go "")
V_PARAMS=$(read_version params/version.go "Version")
echo "tag=$TAG wants $WANT; version/version.go=$V_VERSION; params/version.go=$V_PARAMS"
[[ "$V_VERSION" == "$WANT" ]] || fail "version/version.go is $V_VERSION, tag $TAG wants $WANT"
[[ "$V_PARAMS" == "$WANT" ]] || fail "params/version.go is $V_PARAMS, tag $TAG wants $WANT"

# --- 2. version reported by the binary in the image -------------------------
VERSION_OUT=$(docker run --rm "$IMAGE" version)
echo "$VERSION_OUT"
REPORTED=$(sed -nE 's/^Version:[[:space:]]*([0-9]+\.[0-9]+\.[0-9]+).*/\1/p' <<<"$VERSION_OUT")
[[ -n "$REPORTED" ]] || fail "no 'Version:' line in 'geth version' output"
[[ "$REPORTED" == "$WANT" ]] || fail "image reports $REPORTED, tag $TAG wants $WANT"

# --- 3. fork schedule loaded per network ------------------------------------
check_network() { # <geth flag> <embedded config file>
  local flag="$1" embedded="$2" name loaded
  name="smoke-${flag}-$$"
  [[ -f "$embedded" ]] || fail "$embedded not found"

  CONTAINERS+=("$name")
  docker run -d --name "$name" "$IMAGE" "--${flag}" \
    --datadir /tmp/d --nodiscover --maxpeers 0 --ipcpath /tmp/geth.ipc >/dev/null

  local _
  for _ in $(seq 1 40); do
    if docker exec "$name" test -S /tmp/geth.ipc; then break; fi
    if [[ "$(docker inspect -f '{{.State.Running}}' "$name")" != "true" ]]; then
      docker logs "$name" 2>&1 | tail -30 >&2
      fail "geth --${flag} exited before opening its IPC socket"
    fi
    sleep 3
  done
  docker exec "$name" test -S /tmp/geth.ipc || fail "geth --${flag}: IPC socket not up after 120s"

  loaded=$(docker exec -e HOME=/tmp "$name" geth attach --exec \
    'JSON.stringify(admin.nodeInfo.protocols.eth.config)' /tmp/geth.ipc | tail -1)

  # geth attach prints the JSON string as a quoted JS string literal.
  python3 - "$embedded" "$loaded" "$flag" <<'PY'
import json, sys
embedded_file, loaded_raw, flag = sys.argv[1], sys.argv[2], sys.argv[3]
loaded = json.loads(json.loads(loaded_raw))
cfg = json.load(open(embedded_file))
cfg = cfg.get("config", cfg)
# The embedded JSON mixes key case (e.g. "PragueTime"); compare case-insensitively.
got = {k.lower(): v for k, v in loaded.items()}
want = {k: v for k, v in cfg.items() if k.endswith("Time") or k == "chainId"}
bad = []
for key, value in sorted(want.items()):
    have = got.get(key.lower(), "<missing>")
    status = "ok" if have == value else "MISMATCH"
    print(f"  --{flag:7} {key:26} embedded={value!s:12} loaded={have!s:12} {status}")
    if have != value:
        bad.append(key)
if bad:
    print(f"::error::--{flag}: loaded fork schedule differs from {embedded_file}: {', '.join(bad)}", file=sys.stderr)
    sys.exit(1)
PY
}

check_network spicy config/embedded/spicy.json
check_network chiliz config/embedded/chiliz.json

echo "smoke test passed for $IMAGE ($TAG)"
