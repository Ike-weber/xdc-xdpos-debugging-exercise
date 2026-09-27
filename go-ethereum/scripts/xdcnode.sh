#!/usr/bin/env bash
# =============================================================================
#  XDC MAINNET FAST NODE  —  all-in-one manager
# -----------------------------------------------------------------------------
#  One script does everything. Just type a word after it:
#
#     ./xdcnode.sh setup      First time: downloads the latest snapshot
#                             (with a live progress bar) so your node starts
#                             near the live chain in minutes — not days.
#     ./xdcnode.sh start      Start the node.
#     ./xdcnode.sh status     Pretty sync status: % synced, block, peers.
#     ./xdcnode.sh stop       Stop the node (safely).
#     ./xdcnode.sh restart    Stop, then start.
#     ./xdcnode.sh logs        Watch the live log.
#     ./xdcnode.sh attach      Open the geth console (advanced).
#     ./xdcnode.sh             Show help.
#
#  If anything looks stuck, just re-run the same command — it is safe to repeat
#  (the download resumes where it left off).
# =============================================================================
set -uo pipefail

# ----------------------------- CONFIG (edit if needed) -----------------------
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GETH="$DIR/geth"                 # the node program (provided)
ZSTDDEC="$DIR/zstddec"           # snapshot decompressor (provided)
DATADIR="$DIR/data"
CHAINDATA="$DATADIR/XDC/chaindata"
LOGDIR="$DIR/logs"; LOG="$LOGDIR/node.log"
PIDFILE="$DIR/node.pid"
IPC="$DATADIR/XDC.ipc"
CONFIG="$DIR/config.toml"
SNAP_URL="https://xdc.network/snapshots/xdc168-mainnet-fast-latest.tar.zst"
SNAP="$DIR/snapshot.tar.zst"
RPC_PORT=8565; WS_PORT=8566; P2P_PORT=30306; AUTH_PORT=8567
CACHE=2048; MEMLIMIT="3GiB"
STATS_SECRET="xdc_openscan_stats_2026"

# --- Node-name standard:  host-network-nodeType-OS-location ---
# Each part auto-derives; override by exporting before running, e.g.
#   HOST=xdcone NETWORK=mainnet NODETYPE=fast LOCATION=in ./xdcnode.sh start
HOST="${HOST:-$(hostname -s 2>/dev/null | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9]//g')}"; [ -z "$HOST" ] && HOST="xdcnode"
NETWORK="${NETWORK:-mainnet}"
NODETYPE="${NODETYPE:-fast}"
OSID="$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')"; [ "$OSID" = "darwin" ] && OSID="macos"; [ -z "$OSID" ] && OSID="os"
LOCATION="${LOCATION:-$(curl -s --max-time 5 https://ipinfo.io/country 2>/dev/null | tr '[:upper:]' '[:lower:]' | tr -cd 'a-z')}"; [ -z "$LOCATION" ] && LOCATION="xx"
NODE_NAME="${HOST}-${NETWORK}-${NODETYPE}-${OSID}-${LOCATION}"
ETHSTATS="${NODE_NAME}:${STATS_SECRET}@stats.xdcindia.com:443"
# Trusted fleet peers (already at the chain tip + serve snapshot state)
PEER1="enode://7603346c94fbb6e8f6351062bd9ee784605268dae543bc67cd542fc430dee48b4b733c153a2606f5429ea00a8d900b489a7a6e84eb80416862c466ee3834b105@167.235.13.113:30310"
PEER2="enode://86d5581bf8e7c1fb76677a4245c62587769319fad0986bb4d9ff4c8357f604e8dc4b66b7120dc8fa316c6042e51a0cf2a1921cffad432d1488b134fde3717006@185.180.220.183:30320"

# --- cross-platform helpers (Linux + macOS) ---
# file size in bytes: Linux `stat -c%s`, macOS/BSD `stat -f%z`, fallback wc -c
fsize(){ [ -e "$1" ] || { echo 0; return; }; stat -c%s "$1" 2>/dev/null || stat -f%z "$1" 2>/dev/null || wc -c <"$1" 2>/dev/null | tr -d ' ' || echo 0; }
# sha256: Linux `sha256sum`, macOS `shasum -a 256`
sha256_of(){ if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" 2>/dev/null | awk '{print $1}'; else shasum -a 256 "$1" 2>/dev/null | awk '{print $1}'; fi; }
# zstd decompress to stdout: prefer system `zstd`, else the bundled platform binary
ZDEC(){ if command -v zstd >/dev/null 2>&1; then zstd -dc; else "$ZSTDDEC"; fi; }
# format a seconds count as "Xm YYs" for per-stage timing transparency
fmt_dur(){ local s=${1:-0}; [ "$s" -lt 0 ] 2>/dev/null && s=0; printf '%dm %02ds' "$((s/60))" "$((s%60))"; }

# ----------------------------- PRETTY OUTPUT ---------------------------------
if [ -t 1 ]; then B=$'\033[1m'; G=$'\033[32m'; Y=$'\033[33m'; R=$'\033[31m'; C=$'\033[36m'; D=$'\033[2m'; N=$'\033[0m'; TTY=1; else B=""; G=""; Y=""; R=""; C=""; D=""; N=""; TTY=0; fi
hr(){ printf '%s\n' "──────────────────────────────────────────────────────────────"; }
ok(){   printf '   %s✓%s %s\n' "$G" "$N" "$*"; }
warn(){ printf '   %s•%s %s\n' "$Y" "$N" "$*"; }
err(){  printf '   %s✗%s %s\n' "$R" "$N" "$*"; }
step(){ printf '\n%s▶ %s%s\n' "$B" "$*" "$N"; }
gb(){ awk "BEGIN{printf \"%.2f\", ${1:-0}/1073741824}"; }
banner(){ hr; printf '%s   XDC MAINNET FAST NODE%s   %s(%s)%s\n' "$B" "$N" "$D" "$(basename "$DIR")" "$N"; printf '%s   node name:%s %s\n' "$D" "$N" "$NODE_NAME"; hr; }

# progress bar — TTY: live in-place; non-TTY: one tidy line per tick
draw_bar(){
  local cur=$1 total=$2 label=${3:-}
  local pct=0; [ "${total:-0}" -gt 0 ] 2>/dev/null && pct=$(( cur*100/total )); [ "$pct" -gt 100 ] && pct=100
  local width=32
  local filled=$(( pct*width/100 ))
  local i=0 bar=""
  while [ "$i" -lt "$filled" ]; do bar="$bar#"; i=$((i+1)); done
  while [ "$i" -lt "$width" ]; do bar="$bar."; i=$((i+1)); done
  if [ "$TTY" = 1 ]; then
    printf '\r   %s[%s]%s %3d%%  %s / %s GB  %s   ' "$C" "$bar" "$N" "$pct" "$(gb "$cur")" "$(gb "$total")" "$label"
  else
    printf '   [%s] %3d%%  %s / %s GB  %s\n' "$bar" "$pct" "$(gb "$cur")" "$(gb "$total")" "$label"
  fi
}

is_running(){ [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE" 2>/dev/null)" 2>/dev/null; }
rpc(){ curl -s -m6 -X POST -H 'Content-Type:application/json' --data "{\"jsonrpc\":\"2.0\",\"method\":\"$1\",\"params\":${2:-[]},\"id\":1}" "http://127.0.0.1:$RPC_PORT" 2>/dev/null; }
h2d(){ local h; h=$(echo "$1"|grep -oE '0x[0-9a-f]+'|head -1); [ -n "$h" ] && printf '%d' "$h" || echo ""; }

ensure_config(){
  [ -f "$CONFIG" ] && return 0
  cat > "$CONFIG" <<TOML
[Node.P2P]
StaticNodes  = ["$PEER1","$PEER2"]
TrustedNodes = ["$PEER1","$PEER2"]
TOML
}

# ----------------------------- SETUP -----------------------------------------
cmd_setup(){
  banner
  local SETUP_T0; SETUP_T0=$(date +%s); local DL_SECS=0 EX_SECS=0
  step "Step 1 of 4  —  Check for existing chain data"
  if [ -d "$CHAINDATA" ] && [ -n "$(ls -A "$CHAINDATA" 2>/dev/null)" ]; then
    ok "Chain data already here ($(du -sh "$CHAINDATA" 2>/dev/null|cut -f1)). No download needed."
    printf '\n   %sNext step:%s %s./xdcnode.sh start%s\n' "$B" "$N" "$G" "$N"; return 0
  fi
  [ -x "$GETH" ] || { err "node program 'geth' missing in this folder."; exit 1; }
  command -v zstd >/dev/null 2>&1 || [ -x "$ZSTDDEC" ] || { err "need a zstd decompressor: install 'zstd' or place 'zstddec' in this folder."; exit 1; }
  warn "No chain data yet. Downloading the latest snapshot so your node starts"
  warn "near the live chain (a few minutes) instead of from block 0 (many days)."

  step "Step 2 of 4  —  Download latest snapshot"
  printf '   %ssource:%s %s\n' "$D" "$N" "$SNAP_URL"
  local total; total=$(curl -sIL --retry 3 -A 'Mozilla/5.0' "$SNAP_URL" | tr -d '\r' | awk 'tolower($1)=="content-length:"{v=$2} END{print v+0}')
  printf '   %ssize:%s ~%s GB   %s(resumable — safe to re-run if interrupted)%s\n' "$D" "$N" "$(gb "$total")" "$D" "$N"
  local DL_T0; DL_T0=$(date +%s)
  curl -fL -C - --retry 30 --retry-delay 5 --retry-all-errors -A 'Mozilla/5.0' -s -o "$SNAP" "$SNAP_URL" &
  local dl=$!
  local pc; pc=$(fsize "$SNAP"); local pt; pt=$(date +%s)
  while kill -0 "$dl" 2>/dev/null; do
    local cur; cur=$(fsize "$SNAP")
    local now; now=$(date +%s); local dt=$(( now-pt )); local dc=$(( cur-pc )); local sp=0
    [ "$dt" -gt 0 ] && [ "$dc" -gt 0 ] && sp=$(( dc/dt ))
    local mb; mb=$(awk "BEGIN{printf \"%.0f\", $sp/1048576}")
    local eta="--"; [ "$sp" -gt 0 ] && [ "${total:-0}" -gt "$cur" ] && eta="$(( (total-cur)/sp/60 ))m"
    draw_bar "$cur" "$total" "${mb} MB/s  eta ${eta}"
    pc=$cur; pt=$now
    sleep 3
  done
  wait "$dl" || { echo; err "Download failed. Re-run ./xdcnode.sh setup (it resumes)."; exit 1; }
  draw_bar "$total" "$total" "done"; echo
  ok "Downloaded $(gb "$(fsize "$SNAP")") GB"
  DL_SECS=$(( $(date +%s) - DL_T0 )); printf '   %s⏱  download stage: %s%s\n' "$D" "$(fmt_dur "$DL_SECS")" "$N"

  # Integrity check — catches a corrupt/mismatched file (e.g. a resumed partial
  # left over from a different snapshot) BEFORE we waste minutes extracting it.
  local want; want=$(curl -s -A 'Mozilla/5.0' --max-time 25 "${SNAP_URL}.sha256" 2>/dev/null | grep -oE '[0-9a-f]{64}' | head -1)
  if [ -n "$want" ]; then
    step "Verifying download integrity (sha256)…"
    local got; got=$(sha256_of "$SNAP")
    if [ "$got" != "$want" ]; then
      err "Integrity check FAILED — file is corrupt (likely a leftover partial from a different snapshot)."
      warn "Deleting and re-downloading fresh (no resume)…"
      rm -f "$SNAP"
      curl -fL --retry 30 --retry-delay 5 --retry-all-errors -A 'Mozilla/5.0' -s -o "$SNAP" "$SNAP_URL" || { err "Re-download failed."; exit 1; }
      got=$(sha256_of "$SNAP")
      [ "$got" = "$want" ] || { err "Still corrupt after fresh download — aborting (check network)."; exit 1; }
    fi
    ok "Integrity verified — sha256 matches"
  else
    warn "No published checksum found — skipping integrity check"
  fi

  step "Step 3 of 4  —  Restore snapshot (extract — takes a few minutes)"
  local EX_T0; EX_T0=$(date +%s)
  rm -rf "$DIR/_stage"; mkdir -p "$DIR/_stage"
  local expect=$(( total*3 ))
  ZDEC < "$SNAP" | tar -xf - -C "$DIR/_stage" &
  local xp=$!
  while kill -0 "$xp" 2>/dev/null; do
    local xs; xs=$(du -sk "$DIR/_stage" 2>/dev/null | cut -f1); xs=$(( ${xs:-0}*1024 ))
    draw_bar "$xs" "$expect" "extracting…"
    sleep 3
  done
  wait "$xp" || { echo; err "Extraction failed."; exit 1; }
  echo
  local cd; cd=$(find "$DIR/_stage" -maxdepth 3 -type d -name chaindata | head -1)
  [ -z "$cd" ] && ls "$DIR/_stage"/*.sst >/dev/null 2>&1 && cd="$DIR/_stage"
  [ -z "$cd" ] && { err "chaindata not found inside snapshot."; exit 1; }
  mkdir -p "$DATADIR/XDC"; rm -rf "$CHAINDATA"; mv "$cd" "$CHAINDATA"
  rm -rf "$DIR/_stage" "$SNAP"
  ok "Restored to chaindata ($(du -sh "$CHAINDATA" 2>/dev/null|cut -f1))"
  EX_SECS=$(( $(date +%s) - EX_T0 )); printf '   %s⏱  extract stage:  %s%s\n' "$D" "$(fmt_dur "$EX_SECS")" "$N"

  step "Step 4 of 4  —  Done"
  ok "Your node is set up with a recent snapshot of the XDC mainnet."
  local SETUP_SECS=$(( $(date +%s) - SETUP_T0 ))
  printf '   %s⏱  setup total: %s%s   %s(download %s · extract %s)%s\n' "$B" "$(fmt_dur "$SETUP_SECS")" "$N" "$D" "$(fmt_dur "$DL_SECS")" "$(fmt_dur "$EX_SECS")" "$N"
  # stamp setup-complete time so cmd_start/status can show the catch-up-to-tip stage
  date +%s > "$DIR/.setup_done_at" 2>/dev/null
  printf '\n   %sNext step:%s  %s./xdcnode.sh start%s     %sthen:%s ./xdcnode.sh status\n' "$B" "$N" "$G" "$N" "$D" "$N"
}

# ----------------------------- START / STOP ----------------------------------
cmd_start(){
  banner
  if is_running; then ok "Already running (pid $(cat "$PIDFILE")). See: ./xdcnode.sh status"; return 0; fi
  if [ ! -d "$CHAINDATA" ] || [ -z "$(ls -A "$CHAINDATA" 2>/dev/null)" ]; then
    warn "No chain data found — running first-time setup automatically (download + restore snapshot)…"
    cmd_setup || { err "Setup failed; not starting."; return 1; }
  fi
  ensure_config; mkdir -p "$LOGDIR"; ulimit -n 65536 2>/dev/null || true
  step "Starting the node…"
  XDC_SNAP_HEAL=1 GOMEMLIMIT="$MEMLIMIT" nohup "$GETH" \
    --datadir "$DATADIR" --config "$CONFIG" --xdcmainnet --port "$P2P_PORT" \
    --syncmode fast --gcmode full --state.scheme hash \
    --http --http.addr 127.0.0.1 --http.port "$RPC_PORT" --http.api eth,net,web3,debug,admin,XDPoS \
    --ws --ws.port "$WS_PORT" --authrpc.addr 127.0.0.1 --authrpc.port "$AUTH_PORT" \
    --cache "$CACHE" --maxpeers 50 --verbosity 3 \
    --ethstats "$ETHSTATS" --ipcpath "$IPC" >> "$LOG" 2>&1 &
  echo $! > "$PIDFILE"; sleep 6
  if is_running; then ok "Started (pid $(cat "$PIDFILE"))."; date +%s > "$DIR/.started_at" 2>/dev/null
    printf '\n   %sWatch progress:%s %s./xdcnode.sh status%s   %s(shows catch-up time + time-to-tip)%s\n' "$B" "$N" "$G" "$N" "$D" "$N"
  else err "Did not start — check: ./xdcnode.sh logs"; fi
}
cmd_stop(){
  banner
  if ! is_running; then ok "Already stopped."; return 0; fi
  local p; p=$(cat "$PIDFILE"); step "Stopping (pid $p) — allowing a clean save…"
  kill "$p" 2>/dev/null
  local i=0; while [ "$i" -lt 40 ] && kill -0 "$p" 2>/dev/null; do sleep 1; i=$((i+1)); done
  if kill -0 "$p" 2>/dev/null; then kill -9 "$p" 2>/dev/null; warn "Force-stopped."; else ok "Stopped cleanly."; fi
  rm -f "$PIDFILE"
}

# ----------------------------- STATUS ----------------------------------------
cmd_status(){
  banner
  if ! is_running; then
    err "Node is NOT running."
    printf '   %sFirst time?%s %s./xdcnode.sh setup%s   %sAlready set up?%s %s./xdcnode.sh start%s\n' "$B" "$N" "$G" "$N" "$B" "$N" "$G" "$N"; return 0
  fi
  ok "Node is running (pid $(cat "$PIDFILE"))."
  local head peers sy cur high tip
  head=$(h2d "$(rpc eth_blockNumber)"); peers=$(h2d "$(rpc net_peerCount)"); sy=$(rpc eth_syncing)
  step "Sync progress"
  if echo "$sy" | grep -q '"highestBlock"'; then
    cur=$(h2d "$(echo "$sy"|grep -oE '"currentBlock":"0x[0-9a-f]+"')"); high=$(h2d "$(echo "$sy"|grep -oE '"highestBlock":"0x[0-9a-f]+"')")
    draw_bar "${cur:-0}" "${high:-0}" "block ${cur:-0} of ${high:-0}"; [ "$TTY" = 1 ] && echo
    warn "Catching up to the network — please wait."
    local _sa; _sa=$(cat "$DIR/.started_at" 2>/dev/null); [ -n "$_sa" ] && printf '   %s⏱  catching up for %s%s\n' "$D" "$(fmt_dur $(( $(date +%s) - _sa )))" "$N"
  else
    # eth_syncing=false: estimate tip from the best peer head
    tip=$(rpc admin_peers | grep -oE '"head":"0x[0-9a-f]+"' | grep -oE '0x[0-9a-f]+' | while read x; do printf '%d\n' "$x"; done | sort -n | tail -1)
    if [ -n "${head:-}" ] && [ -n "${tip:-}" ] && [ "$tip" -gt 0 ] && [ "$head" -lt "$tip" ]; then
      draw_bar "$head" "$tip" "block $head of ~$tip"; [ "$TTY" = 1 ] && echo
      warn "Almost there — finishing the last blocks."
      local _sa; _sa=$(cat "$DIR/.started_at" 2>/dev/null); [ -n "$_sa" ] && printf '   %s⏱  catching up for %s%s\n' "$D" "$(fmt_dur $(( $(date +%s) - _sa )))" "$N"
    else
      draw_bar 100 100 "at the chain tip"; [ "$TTY" = 1 ] && echo
      ok "SYNCED — your node is at the live chain tip."
      [ -f "$DIR/.tip_reached_at" ] || date +%s > "$DIR/.tip_reached_at" 2>/dev/null
      local _t0 _tr; _t0=$(cat "$DIR/.setup_done_at" 2>/dev/null || cat "$DIR/.started_at" 2>/dev/null); _tr=$(cat "$DIR/.tip_reached_at" 2>/dev/null)
      [ -n "$_t0" ] && [ -n "$_tr" ] && printf '   %s⏱  reached tip in %s (clean dir → tip)%s\n' "$D" "$(fmt_dur $(( _tr - _t0 )))" "$N"
    fi
  fi
  printf '\n   %sBlock height:%s %s        %sConnected peers:%s %s\n' "$B" "$N" "${head:-?}" "$B" "$N" "${peers:-0}"
  printf '   %sCommands:%s start | stop | restart | status | logs | attach\n' "$D" "$N"
}

usage(){
  banner
  printf '   One script does everything. Type a word after it:\n\n'
  printf '   %ssetup%s     First time: download the latest snapshot (live progress bar)\n' "$G" "$N"
  printf '             so your node starts near the live chain in minutes, not days.\n'
  printf '   %sstart%s     Start the node.\n' "$G" "$N"
  printf '   %sstatus%s    Pretty sync status: %% synced, block height, peers.\n' "$G" "$N"
  printf '   %sstop%s      Stop the node safely.\n' "$G" "$N"
  printf '   %srestart%s   Stop, then start.\n' "$G" "$N"
  printf '   %slogs%s      Watch the live log.\n' "$G" "$N"
  printf '   %sattach%s    Open the geth console (advanced).\n' "$G" "$N"
  printf '\n   %sTip:%s if anything looks stuck, re-run the same command — it is safe\n' "$D" "$N"
  printf '   to repeat (downloads resume where they left off).\n'
  printf '\n   %sTypical first run:%s  ./xdcnode.sh setup  →  ./xdcnode.sh start  →  ./xdcnode.sh status\n' "$B" "$N"
}

case "${1:-start}" in
  setup|init|download) cmd_setup ;;
  start|up)            cmd_start ;;
  stop|down)           cmd_stop ;;
  restart)             cmd_stop; cmd_start ;;
  status|info)         cmd_status ;;
  logs|log)            tail -f "$LOG" ;;
  attach|console)      "$GETH" attach "$IPC" ;;
  help|-h|--help|*)     usage ;;
esac
