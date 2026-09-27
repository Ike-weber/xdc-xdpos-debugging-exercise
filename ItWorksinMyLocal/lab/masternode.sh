#!/bin/bash
# masternode.sh - drive the XDPoS masternode contract (0x0...088) for the
# lab: propose | resign | vote | unvote | withdraw | list, plus a few
# read-only diagnostics. Selectors and ABI shapes are the ones verified in
# docs/lab/DESIGN.md §4.
#
# Every write method is sent as an eth_sendTransaction from a prefunded
# account, unlocked either via personal_ (given its raw private key with
# --key -- e.g. a lab candidate-owner account from lab/lab-accounts.env) or
# already unlocked on the node (a producer sealer, --from without --key).
# `list` and the read-only diagnostics use plain eth_call and work against
# ANY client (universal), matching the lab's differential-oracle constraint
# of never depending on XDPoS_*.
#
# Usage:
#   ./masternode.sh propose  <candidateAddr> [opts]
#   ./masternode.sh resign   <candidateAddr> [opts]
#   ./masternode.sh vote     <candidateAddr> [opts]
#   ./masternode.sh unvote   <candidateAddr> <capXDC> [opts]
#   ./masternode.sh withdraw <blockNumber> <index> [opts]
#   ./masternode.sh list     [--rpc URL]
#
#   ./masternode.sh upload-kyc [hash] [opts]
#   ./masternode.sh candidate-cap   <candidateAddr> [--rpc URL]
#   ./masternode.sh is-candidate    <candidateAddr> [--rpc URL]
#   ./masternode.sh candidate-count [--rpc URL]
#   ./masternode.sh max-validators  [--rpc URL]
#   ./masternode.sh withdraw-delay  [--rpc URL]
#
# NOT in docs/lab/DESIGN.md §4 but required empirically, verified live
# against the deployed 0x88 contract (../XDPoSChain/contracts/validator/
# contract/XDCValidator.sol): propose() carries an onlyKYCWhitelisted
# modifier (`KYCString[msg.sender].length != 0 || ownerToCandidate[msg.sender
# ].length > 0`) -- a brand-new owner account must call
# uploadKYC(string) at least once before its first propose(), or propose()
# reverts (status 0x0) with no revert reason surfaced over RPC. `propose`
# below does NOT call this for you (kept a separate, explicit step so
# scenario files can exercise/omit it deliberately); call `upload-kyc` first
# for any lab candidate-owner account that has never proposed before.
# uploadKYC's selector (f5c95125) isn't in DESIGN §4 either; verified here by
# cross-checking two independent keccak256 implementations
# (pycryptodome and eth_hash) against `uploadKYC(string)`.
#
# Options (propose/resign/vote/unvote/withdraw/upload-kyc):
#   --key HEXPRIVKEY    raw private key to import + unlock on the node (a lab
#                       candidate-owner account from lab/lab-accounts.env).
#                       Pass --from alongside it (see below) when reusing the
#                       same account across multiple calls (e.g. upload-kyc
#                       then propose) -- re-importing an already-imported key
#                       errors on this client, and --from sidesteps that by
#                       supplying the address up front instead of relying on
#                       the (one-time-only) import response for it.
#   --key-stdin          same as --key, but reads the raw private key from
#                       stdin (one line) instead of argv. Added for
#                       lab/onboard-masternode.sh (ItWorksinMyLocal#62), which
#                       drives brand-new owner/coinbase keys that must never
#                       appear on a command line (visible to any other user
#                       via `ps`/`/proc/<pid>/cmdline`, and to shell history).
#                       --key is kept as-is for existing lab-accounts.env-based
#                       callers (scenario files, etc.) -- this is purely
#                       additive, same _resolve_sender path either way.
#   --from ADDR          an address already unlocked on the node (e.g. a
#                       producer sealer account), used instead of --key; or,
#                       combined with --key/--key-stdin, the known address for
#                       that key (recommended -- see above)
#   --rpc URL            JSON-RPC endpoint (default: the producer, node1)
#   --value XDC          XDC amount to send with propose/vote (defaults:
#                       propose=11000000, vote=25000 -- just above the
#                       genesis minCandidateCap/minVoterCap)
#   --gas N              gas limit (default 2000000)
#   --unlock-secs N      personal_unlockAccount duration (default 300)

_SELF_DIR="$(cd "$(dirname "$0")" && pwd)" || exit 1
_SELF="$_SELF_DIR/$(basename "$0")"
cd "$_SELF_DIR" || exit 1
# shellcheck source=lib-lab.sh
source ./lib-lab.sh

# Selectors verified in docs/lab/DESIGN.md §4.
SEL_PROPOSE=01267951
SEL_RESIGN=ae6e43f5
SEL_VOTE=6dd7d8ea
SEL_UNVOTE=02aa9be2
SEL_WITHDRAW=441a3e70
# getCandidates() itself is not called here -- `list` reuses
# lab_get_candidates (lib-lab.sh), the single shared ABI-decode
# implementation also used by oracle.sh.
# uploadKYC(string) -- not in DESIGN §4, see the header note above.
SEL_UPLOAD_KYC=f5c95125
SEL_GET_CANDIDATE_CAP=58e7525f
SEL_IS_CANDIDATE=d51b9e93
SEL_CANDIDATE_COUNT=a9a981a3
SEL_MAX_VALIDATOR_NUMBER=d09f1ab4
SEL_CANDIDATE_WITHDRAW_DELAY=d161c767

RPC=""
KEY=""
KEY_STDIN=""
FROM=""
VALUE=""
GAS=2000000
UNLOCK_SECS=300

_usage() { sed -n '2,71p' "$_SELF" | sed 's/^# \{0,1\}//'; }

# Parses the trailing --opt value pairs common to every subcommand.
_parse_common_opts() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --rpc)          RPC=$2; shift 2 ;;
      --key)          KEY=$2; shift 2 ;;
      --key-stdin)    KEY_STDIN=1; shift 1 ;;
      --from)         FROM=$2; shift 2 ;;
      --value)        VALUE=$2; shift 2 ;;
      --gas)          GAS=$2; shift 2 ;;
      --unlock-secs)  UNLOCK_SECS=$2; shift 2 ;;
      *) echo "unknown option: $1" >&2; return 1 ;;
    esac
  done
}

# Prints the sending address: unlocks --key/--key-stdin if given, else --from
# (assumed already unlocked on the node), else errors.
_resolve_sender() {
  local rpc="$1"
  if [ -n "$KEY_STDIN" ]; then
    # Read exactly one line from stdin -- the raw private key never touches
    # argv (see the --key-stdin header note: `ps`/`/proc/<pid>/cmdline`
    # otherwise expose it to every other user on the box).
    local key_from_stdin
    IFS= read -r key_from_stdin || { echo "masternode.sh: --key-stdin given but stdin had no line" >&2; return 1; }
    lab_unlock_key "$rpc" "$key_from_stdin" "$UNLOCK_SECS" "$FROM"
    return $?
  fi
  if [ -n "$KEY" ]; then
    # Passing --from alongside --key (recommended for lab accounts, whose
    # address is already known from lab/lab-accounts.env) makes repeated
    # calls with the same key robust -- see lab_unlock_key's comment.
    lab_unlock_key "$rpc" "$KEY" "$UNLOCK_SECS" "$FROM"
    return $?
  fi
  if [ -n "$FROM" ]; then
    printf '%s\n' "$FROM"
    return 0
  fi
  echo "masternode.sh: need --key <privkey>, --key-stdin, or --from <address>" >&2
  return 1
}

# _send <rpc> <data-hex-no-0x> <value-xdc-or-empty> <gas>: eth_sendTransaction
# to the masternode contract, prints the returned tx hash.
_send() {
  local rpc="$1" data="$2" value_xdc="$3" gas="$4" from value_hex gas_hex txobj
  from=$(_resolve_sender "$rpc") || return 1
  if [ -n "$value_xdc" ] && [ "$value_xdc" != "0" ]; then
    value_hex=$(lab_xdc_to_wei_hex "$value_xdc") || return 1
  else
    value_hex="0x0"
  fi
  gas_hex=$(printf '0x%x' "$gas")
  txobj=$(printf '{"from":"%s","to":"%s","data":"0x%s","value":"%s","gas":"%s"}' \
    "$from" "$LAB_MASTERNODE_CONTRACT" "$data" "$value_hex" "$gas_hex")
  lab_rpc_call "$rpc" eth_sendTransaction "[${txobj}]"
}

cmd_propose() {
  local candidate="${1:-}"
  [ -n "$candidate" ] || { echo "usage: propose <candidateAddr> [opts]" >&2; return 1; }
  shift
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  [ -n "$VALUE" ] || VALUE=11000000
  local word data txh
  word=$(lab_abi_encode_address "$candidate") || return 1
  data="${SEL_PROPOSE}${word}"
  txh=$(_send "$RPC" "$data" "$VALUE" "$GAS") || return 1
  echo "propose($candidate) value=${VALUE}XDC tx=$txh"
}

cmd_resign() {
  local candidate="${1:-}"
  [ -n "$candidate" ] || { echo "usage: resign <candidateAddr> [opts]" >&2; return 1; }
  shift
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local word data txh
  word=$(lab_abi_encode_address "$candidate") || return 1
  data="${SEL_RESIGN}${word}"
  txh=$(_send "$RPC" "$data" "" "$GAS") || return 1
  echo "resign($candidate) tx=$txh"
}

cmd_vote() {
  local candidate="${1:-}"
  [ -n "$candidate" ] || { echo "usage: vote <candidateAddr> [opts]" >&2; return 1; }
  shift
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  [ -n "$VALUE" ] || VALUE=25000
  local word data txh
  word=$(lab_abi_encode_address "$candidate") || return 1
  data="${SEL_VOTE}${word}"
  txh=$(_send "$RPC" "$data" "$VALUE" "$GAS") || return 1
  echo "vote($candidate) value=${VALUE}XDC tx=$txh"
}

cmd_unvote() {
  local candidate="${1:-}" cap_xdc="${2:-}"
  [ -n "$candidate" ] && [ -n "$cap_xdc" ] || { echo "usage: unvote <candidateAddr> <capXDC> [opts]" >&2; return 1; }
  shift 2
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local addr_word cap_wei cap_word data txh
  addr_word=$(lab_abi_encode_address "$candidate") || return 1
  cap_wei=$(python3 -c "import sys; print(int(sys.argv[1]) * 10**18)" "$cap_xdc") || return 1
  cap_word=$(lab_abi_encode_uint256 "$cap_wei") || return 1
  data="${SEL_UNVOTE}${addr_word}${cap_word}"
  txh=$(_send "$RPC" "$data" "" "$GAS") || return 1
  echo "unvote($candidate, ${cap_xdc}XDC) tx=$txh"
}

cmd_withdraw() {
  local blk="${1:-}" idx="${2:-}"
  [ -n "$blk" ] && [ -n "$idx" ] || { echo "usage: withdraw <blockNumber> <index> [opts]" >&2; return 1; }
  shift 2
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local blk_word idx_word data txh
  blk_word=$(lab_abi_encode_uint256 "$blk") || return 1
  idx_word=$(lab_abi_encode_uint256 "$idx") || return 1
  data="${SEL_WITHDRAW}${blk_word}${idx_word}"
  txh=$(_send "$RPC" "$data" "" "$GAS") || return 1
  echo "withdraw(block=$blk, index=$idx) tx=$txh"
}

cmd_list() {
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local set_str
  set_str=$(lab_get_candidates "$RPC") || return 1
  if [ -z "$set_str" ]; then
    echo "(no candidates)"
    return 0
  fi
  printf '%s\n' "$set_str" | tr ',' '\n'
}

# uploadKYC(string): required before a brand-new owner's first propose()
# (see the header note); the hash content itself is not checked on-chain
# beyond being non-empty, so a placeholder is fine for lab purposes.
cmd_upload_kyc() {
  local hash="lab-kyc"
  case "${1:-}" in
    ""|--*) : ;;           # no positional hash given, keep the default
    *) hash="$1"; shift ;;
  esac
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local word data txh
  word=$(lab_abi_encode_string "$hash") || return 1
  data="${SEL_UPLOAD_KYC}${word}"
  txh=$(_send "$RPC" "$data" "" "$GAS") || return 1
  echo "uploadKYC(\"$hash\") tx=$txh"
}

cmd_candidate_cap() {
  local addr="${1:-}"
  [ -n "$addr" ] || { echo "usage: candidate-cap <address> [--rpc URL]" >&2; return 1; }
  shift
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local word raw
  word=$(lab_abi_encode_address "$addr") || return 1
  raw=$(lab_call_0x88 "$RPC" "${SEL_GET_CANDIDATE_CAP}${word}") || return 1
  python3 -c "import sys; print(int(sys.argv[1], 16))" "$raw"
}

cmd_is_candidate() {
  local addr="${1:-}"
  [ -n "$addr" ] || { echo "usage: is-candidate <address> [--rpc URL]" >&2; return 1; }
  shift
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local word raw val
  word=$(lab_abi_encode_address "$addr") || return 1
  raw=$(lab_call_0x88 "$RPC" "${SEL_IS_CANDIDATE}${word}") || return 1
  val=$(python3 -c "import sys; print(int(sys.argv[1], 16))" "$raw")
  if [ "$val" = "0" ]; then echo "false"; else echo "true"; fi
}

cmd_candidate_count() {
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local raw
  raw=$(lab_call_0x88 "$RPC" "$SEL_CANDIDATE_COUNT") || return 1
  python3 -c "import sys; print(int(sys.argv[1], 16))" "$raw"
}

cmd_max_validators() {
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local raw
  raw=$(lab_call_0x88 "$RPC" "$SEL_MAX_VALIDATOR_NUMBER") || return 1
  python3 -c "import sys; print(int(sys.argv[1], 16))" "$raw"
}

cmd_withdraw_delay() {
  _parse_common_opts "$@" || return 1
  [ -n "$RPC" ] || RPC=$(lab_rpc oldxdc)
  local raw
  raw=$(lab_call_0x88 "$RPC" "$SEL_CANDIDATE_WITHDRAW_DELAY") || return 1
  python3 -c "import sys; print(int(sys.argv[1], 16))" "$raw"
}

main() {
  local cmd="${1:-}"
  case "$cmd" in
    ""|-h|--help) _usage; exit 0 ;;
  esac
  shift
  case "$cmd" in
    propose)          cmd_propose "$@" ;;
    resign)           cmd_resign "$@" ;;
    vote)             cmd_vote "$@" ;;
    unvote)           cmd_unvote "$@" ;;
    withdraw)         cmd_withdraw "$@" ;;
    list)             cmd_list "$@" ;;
    upload-kyc)       cmd_upload_kyc "$@" ;;
    candidate-cap)    cmd_candidate_cap "$@" ;;
    is-candidate)     cmd_is_candidate "$@" ;;
    candidate-count)  cmd_candidate_count "$@" ;;
    max-validators)   cmd_max_validators "$@" ;;
    withdraw-delay)   cmd_withdraw_delay "$@" ;;
    *) echo "unknown command: $cmd" >&2; _usage >&2; exit 1 ;;
  esac
}

main "$@"
