#!/bin/bash
# Print a plain-language summary of the CURRENT network, read live from
# genesis/genesis.json (+ .env and network-roles.env). Nothing is hardcoded --
# run it after any ./setup.sh / ./gen-genesis.sh and it reflects that network.
#
# Usage: ./network-info.sh [path/to/genesis.json]

cd "$(dirname "$0")" || exit 1
source ./lib.sh
# Only SKYNET_BASE_URL is used from here (exported below for the python block);
# same optional-config pattern join.sh and run.sh use.
# shellcheck disable=SC1091  # local, operator-editable config; nothing to lint here
[ -f ./network.env ] && . ./network.env
export SKYNET_BASE_URL
GENESIS="${1:-genesis/genesis.json}"
XDC="$XDC_BIN"
[ -f "$GENESIS" ] || { echo "genesis not found: $GENESIS" >&2; exit 1; }

# Derive the addresses of the private keys held in .env (accounts you control).
HELD=""
if [ -f .env ] && [ -x "$XDC" ]; then
  # shellcheck disable=SC2163  # intentional: $_kv holds a "NAME=VALUE" pair from .env, not a var name
  while IFS= read -r _kv; do export "$_kv"; done < <(grep -E '^PRIVATE_KEY_[0-9]+=' .env 2>/dev/null)
  TMP=$(mktemp -d); : > "$TMP/pw"; trap 'rm -rf "$TMP"' EXIT
  i=1
  while :; do
    var="PRIVATE_KEY_$i"; pk="${!var}"; [ -z "$pk" ] && break
    a=$("$XDC" account import --password "$TMP/pw" --datadir "$TMP/k$i" <(printf '%s' "$pk") 2>/dev/null \
          | grep -oE 'xdc[0-9a-fA-F]{40}' | head -1 | sed 's/^xdc//' | tr '[:upper:]' '[:lower:]')
    HELD="$HELD $a"
    i=$((i+1))
  done
fi

ROLES=""
[ -f network-roles.env ] && ROLES=$(cat network-roles.env)

GENESIS="$GENESIS" HELD="$HELD" ROLES="$ROLES" python3 <<'PY'
import json, os
g=json.load(open(os.environ['GENESIS']))
cfg=g['config']; xd=cfg['XDPoS']
held=set(os.environ.get('HELD','').split())
roles={}
for line in os.environ.get('ROLES','').splitlines():
    if '=' in line:
        k,v=line.split('=',1); roles[k]=v.strip()

def to_xdc(hexbal):
    n=int(hexbal,16); v=n/10**18
    return format(v, ',.0f') if v<1e15 else ('%.2e'%v)

# masternodes from extraData
ed=g['extraData'][2:]; body=ed[64:len(ed)-130]
mns=[body[i:i+40].lower() for i in range(0,len(body),40)]

# role label map
label={}
for m in mns: label[m]='Masternode (produces blocks)'
def add(addr,name):
    if addr: label[addr.lower().replace('0x','')]=name
add(cfg.get('XDPoS',{}).get('foudationWalletAddr'),'Foundation wallet')
add(roles.get('OWNER'),'Masternode owner (manages validators)')
add(roles.get('SWAP'),'Swap wallet')
add(roles.get('PREFUND'),'Pre-funded account')
for o in roles.get('FOUNDATION_OWNERS','').split(','):
    if o: label.setdefault(o.lower(),'Foundation multisig approver')
for o in roles.get('TEAM_OWNERS','').split(','):
    if o: label.setdefault(o.lower(),'Team multisig approver')
sysc={'0000000000000000000000000000000000000068':'Built-in contract (block signer / config)',
 '0000000000000000000000000000000000000088':'Built-in contract (validator / masternodes)',
 '0000000000000000000000000000000000000089':'Built-in contract',
 '0000000000000000000000000000000000000090':'Built-in contract',
 '0000000000000000000000000000000000000099':'Built-in contract (foundation/team funds)'}
label.update(sysc)

line='='*70
print(line)
print(" NETWORK SUMMARY  (read live from %s)"%os.environ['GENESIS'])
print(line)
print(" Network        : %s (private, local only)"%roles.get('NETWORK','local'))
print(" Chain ID       : %s"%cfg['chainId'])
# Keyed on the genesis's OWN chainId, so this summary can never advertise a
# dashboard for a different network than the one it just described.
print(" Skynet stats   : %s?net=%s"%(
      os.environ.get('SKYNET_BASE_URL','https://skynet.xdcindia.com/stats'),
      cfg['chainId']))
print(" Coinbase       : %s"%g.get('coinbase','-'))
print(" Block time     : %ss   Epoch: %s   Gap: %s   Block reward: %s XDC"%(
      xd['period'],xd['epoch'],xd['gap'],xd['reward']))
print()
print(" WHO PRODUCES BLOCKS (masternodes):")
for i,m in enumerate(mns,1):
    you=' <- YOU control this (key in .env)' if m in held else ''
    print("   %d. 0x%s%s"%(i,m,you))
print()
print(" KEY ROLES:")
fw=cfg.get('XDPoS',{}).get('foudationWalletAddr','')
if fw: print("   Foundation wallet   : %s"%fw)
if roles.get('OWNER'):
    o=roles['OWNER'].lower()
    print("   Masternode owner    : 0x%s%s"%(o,' [yours]' if o in held else ' (placeholder)'))
if roles.get('SWAP'):
    print("   Swap wallet         : 0x%s"%roles['SWAP'].lower())
print()
print(" MONEY AT START (genesis balances):")
total=0; rows=[]
for a,v in g['alloc'].items():
    bal=int(v.get('balance','0x0'),16); total+=bal
    if bal>0 or a.lower() in label:
        rows.append((bal,a.lower()))
rows.sort(reverse=True)
for bal,a in rows:
    if bal==0 and a not in label: continue
    who=label.get(a,'(other account)')
    mine=' [yours]' if a in held else ''
    print("   0x%s  %20s XDC  - %s%s"%(a,to_xdc(hex(bal)),who,mine))
print()
print(" TOTAL supply at start : %s XDC"%to_xdc(hex(total)))
print(line)
if not held:
    print(" NOTE: no .env keys detected - can't show which accounts you control.")
else:
    print(" You hold the private keys for %d account(s) (marked YOU / [yours])."%len(held))
print(" Accounts WITHOUT a [yours] mark are placeholders - you don't have their")
print(" private keys, so their balances aren't spendable by you. That's fine for")
print(" a local test network.")
print(line)
PY
