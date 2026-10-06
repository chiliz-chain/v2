#!/bin/bash
# Dump every epoch-block header (number, time, extra) since Snake8 for one network as JSONL.
# usage: dump-epochs.sh <rpc-url> <epoch> <snake8Time> <headNumber> <out.jsonl>
set -u
set -o pipefail
rpc=$1; epoch=$2; snake8=$3; head=$4; out=$5
: > "$out"
lastk=$((head / epoch))
# find first epoch index whose block time >= snake8 (binary search on k).
# A failed probe must abort: treating it as "before the fork" silently walks the
# search past the boundary and the dump then starts after Snake8, losing exactly
# the headers it claims to cover.
lo=1; hi=$lastk
while [ $lo -lt $hi ]; do
  mid=$(((lo+hi)/2)); n=$((mid*epoch))
  t=$(curl -sf --max-time 20 -X POST -H 'content-type: application/json' --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_getBlockByNumber\",\"params\":[\"$(printf '0x%x' $n)\",false]}" "$rpc" | jq -r '.result.timestamp' | xargs -I{} printf '%d' {}) \
    || { echo "probe k=$mid (block $n): request failed" >&2; exit 1; }
  case "$t" in ''|*[!0-9]*) echo "probe k=$mid (block $n): non-numeric timestamp '$t'" >&2; exit 1;; esac
  if [ "$t" -ge "$snake8" ]; then hi=$mid; else lo=$((mid+1)); fi
done
# start two epochs earlier to include the boundary
start=$((lo-2)); [ $start -lt 1 ] && start=1
echo "first epoch index >= snake8: $lo; dumping k=$start..$lastk" >&2
k=$start
while [ $k -le $lastk ]; do
  batch="["; sep=""
  for ((j=0; j<50 && k+j<=lastk; j++)); do
    n=$(((k+j)*epoch))
    batch+="$sep{\"jsonrpc\":\"2.0\",\"id\":$((k+j)),\"method\":\"eth_getBlockByNumber\",\"params\":[\"$(printf '0x%x' $n)\",false]}"; sep=","
  done
  batch+="]"
  want=$j
  resp=$(curl -sf --max-time 60 -X POST -H 'content-type: application/json' --data "$batch" "$rpc") \
    || { echo "batch k=$k: curl failed" >&2; exit 1; }
  got=$(printf '%s' "$resp" | jq '[.[] | select(.result != null)] | length')
  if [ "$got" != "$want" ]; then
    echo "batch k=$k: requested $want headers, got $got results (errors: $(printf '%s' "$resp" | jq -c '[.[] | select(.error != null) | .error] | .[0:3]'))" >&2
    exit 1
  fi
  # JSON-RPC permits a batch response in any order (spec 6, "Batch"). The replay
  # requires strictly contiguous ascending epoch headers, so sort before writing —
  # otherwise a reordering server makes a complete dump abort with
  # "epoch headers not contiguous".
  printf '%s' "$resp" | jq -c '[.[] | select(.result != null) | {number: .result.number, time: .result.timestamp, extra: .result.extraData}] | sort_by((.number | ltrimstr("0x") | ascii_downcase | length), (.number | ascii_downcase)) | .[]' >> "$out"
  k=$((k+50))
done
echo "done: $(wc -l < "$out") headers in $out" >&2
