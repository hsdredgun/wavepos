#!/bin/bash
# Sweep the Wavelength wallet to cold storage above a threshold.
# Run it from wave-sweep.timer. Set ADDR to your own on-chain address.
set -euo pipefail

ADDR=bc1qREPLACE_WITH_YOUR_COLD_STORAGE_ADDRESS
THRESHOLD=150000          # sats: start trying to sweep from here
MAX_FEE_PPM=2000          # normal fee limit
FORCE_AT=250000           # sats: near the 300k cap, accept a higher fee
FORCE_MAX_FEE_PPM=10000   # fee limit once FORCE_AT is reached
D=/root/.waved/data/mainnet
WAVECLI=/root/go/bin/wavecli

if [[ $ADDR == *REPLACE* ]]; then
  echo "set ADDR in $0 first"; exit 1
fi

wcli() {
  "$WAVECLI" --rpcserver=127.0.0.1:10029 \
    --tlscertpath=$D/tls.cert --macaroonpath=$D/admin.macaroon \
    --no-input "$@"
}

bal=$(wcli --json balance)
conf=$(jq -r '.confirmed_sat | tonumber' <<<"$bal")
out=$(jq -r '.pending_out_sat | tonumber' <<<"$bal")

if (( out > 0 )); then
  echo "sweep already pending ($out sats out), skipping"; exit 0
fi
if (( conf < THRESHOLD )); then
  echo "balance $conf < $THRESHOLD, nothing to do"; exit 0
fi

limit=$MAX_FEE_PPM
if (( conf >= FORCE_AT )); then
  limit=$FORCE_MAX_FEE_PPM
fi

prev=$(wcli --json send "$ADDR" --onchain --sweep-all --amt 0 --dry-run)
fee=$(jq -r '.preview.expected_fee_sat // empty' <<<"$prev")
if [[ -z $fee ]]; then
  echo "fee unknown, not sweeping: $prev"; exit 1
fi
ppm=$(( fee * 1000000 / conf ))
if (( ppm > limit )); then
  echo "fee $fee sats ($ppm ppm) > $limit ppm on $conf, not sweeping"; exit 0
fi

echo "sweeping $conf sats (fee $fee, $ppm ppm) to $ADDR"
wcli --json send "$ADDR" --onchain --sweep-all --amt 0 --yes
