#!/usr/bin/env python3
"""
gen-genesis-a97m.py  ADDR1 ADDR2 ADDR3 OUT_FILE

Generate devnet genesis.json for devnet-a97m with MasternodeVotingSMC storage.
Slot formulas match XDPoSChain/core/state/state_reader.go exactly.
"""
import sys, json
from Crypto.Hash import keccak as _keccak

def keccak256(data: bytes) -> bytes:
    k = _keccak.new(digest_bits=256)
    k.update(data)
    return k.digest()

# common.Address.Hash() -> right-aligned address in 32 bytes (left zero-padded)
def addr_hash(addr: str) -> bytes:
    return bytes.fromhex(addr.lower().replace('0x','').zfill(64))

def uint256_b(n: int) -> bytes:
    return n.to_bytes(32, 'big')

def to_hash(n: int) -> bytes:
    return n.to_bytes(32, 'big')

def hex_slot(b: bytes) -> str:
    return '0x' + b.hex()

def hex_val(n: int) -> str:
    return '0x' + hex(n)[2:].zfill(64)

# GetLocMappingAtKey(key common.Hash, slot uint64):
#   keccak256(key.Bytes() ++ slotHash.Bytes())
def loc_mapping(key_hash: bytes, slot: int) -> bytes:
    slot_hash = uint256_b(slot)
    return keccak256(key_hash + slot_hash)

# GetLocDynamicArrAtElement(slotHash, index, elementSize):
#   keccak256(slotHash) + index * elementSize
def loc_dyn_arr_elem(slot_hash: bytes, index: int, elem_size: int = 1) -> bytes:
    base = int.from_bytes(keccak256(slot_hash), 'big')
    return to_hash(base + index * elem_size)

def main():
    if len(sys.argv) != 5:
        sys.exit(f"Usage: {sys.argv[0]} ADDR1 ADDR2 ADDR3 OUT_FILE")
    addr1, addr2, addr3 = sys.argv[1], sys.argv[2], sys.argv[3]
    out_file = sys.argv[4]
    addrs = [addr1, addr2, addr3]

    # extraData: 32B vanity + 3*20B addrs + 65B seal
    vanity = '0' * 64
    seal   = '0' * 130
    addr_hex = ''.join(a.lower().replace('0x','') for a in addrs)
    extra_data = '0x' + vanity + addr_hex + seal

    storage = {}

    def S(slot_bytes: bytes, val: int):
        storage[hex_slot(slot_bytes)] = hex_val(val)

    def S_addr(slot_bytes: bytes, addr: str):
        storage[hex_slot(slot_bytes)] = '0x' + addr.lower().replace('0x','').zfill(64)

    # ---- Simple scalar slots (from slotValidatorMapping) ----
    # candidates array length at slot 8
    S(uint256_b(8), 3)
    # candidateCount at slot 9 (not critical but mirrors genesis)
    S(uint256_b(9), 3)
    # ownerCount at slot 10
    S(uint256_b(10), 3)
    # minCandidateCap at slot 11 = 1 ether
    S(uint256_b(11), 10**18)
    # minVoterCap at slot 12 = 0.1 ether
    S(uint256_b(12), 10**17)
    # maxValidatorNumber at slot 13 = 150
    S(uint256_b(13), 150)
    # candidateWithdrawDelay at slot 14
    S(uint256_b(14), 1296000)
    # voterWithdrawDelay at slot 15
    S(uint256_b(15), 432000)

    # ---- candidates[] dynamic array at slot 8 ----
    slot8_hash = uint256_b(8)
    for i, addr in enumerate(addrs):
        elem_slot = loc_dyn_arr_elem(slot8_hash, i, 1)
        S_addr(elem_slot, addr)

    # ---- validatorsState mapping at slot 1 ----
    # struct ValidatorsState { address owner; bool isActive; uint256 cap; mapping(address=>uint256) voters }
    # Solidity storage: slot+0 = packed(owner+isActive), slot+1 = cap, slot+2 = voters mapping base
    for addr in addrs:
        loc = loc_mapping(addr_hash(addr), 1)
        loc_int = int.from_bytes(loc, 'big')
        # slot+0: packed owner(20B) + isActive(bool)
        # Storage layout: lowest slot = rightmost bytes
        # owner is address (20B), isActive is bool (1B) packed after owner
        # In Solidity, struct fields are packed right-to-left:
        #   slot+0, bits 0-159: owner address
        #   slot+0, bit 160: isActive bool
        owner_int = int(addr.lower().replace('0x',''), 16)
        packed = owner_int | (1 << 160)
        storage[hex_slot(loc)] = hex_val(packed)
        # slot+1: cap = 1 ether
        storage[hex_slot(to_hash(loc_int + 1))] = hex_val(10**18)

    # ---- voters mapping at slot 2 ----
    # mapping(address => address[]) voters;
    # voters[candidate].length, then voters[candidate][i] elements
    # NOT the same as the struct slot 2 above — this is a top-level mapping
    # Actually in the contract source:
    #   mapping(address => address[]) voters is at slot 2 (slotValidatorMapping["voters"] = 2)
    # But UpdateM1 uses GetVoterCap which reads from validatorsState slot 2 (different!)
    # For UpdateM1, what matters is:
    #   1. stateDB.GetCandidates() -> reads candidates[] at slot 8
    #   2. validator.GetCandidateCap(opts, candidate) -> reads from CONTRACT via EVM call
    #      This calls the smart contract's getCandidateCap function via Ethereum ABI
    # So we need the contract to return non-zero caps when called via EVM.
    # The EVM reads from storage, so we need the storage slots that the contract reads.
    # 
    # Looking at the contract source embedded in 02-gen-genesis.sh:
    # The contract stores candidate data in mapping(address => Validator) validatorsState at slot 1
    # struct Validator { address owner; bool isActive; uint256 cap; mapping(address=>uint256) voters }
    # cap is at slot+1 relative to the mapping key base.
    # We already set those above.

    # Also need: GetCandidateCap via the ABI call reads from stateDB which we set above.
    # But wait - UpdateM1 calls validator.GetCandidateCap(opts, candidate) which goes through
    # the EVM executing the contract's getCandidateCap function, NOT stateDB.GetCandidateCap.
    # The contract's getCandidateCap reads from its own storage slot 1.
    # We already set slot 1 correctly above.

    print(f"[gen-genesis] Storage slots: {len(storage)}", file=sys.stderr)

    # Full MN_CODE from genesis.json (we'll read it from the original genesis)
    # Use the original genesis code but replace storage/addresses
    ALLOC_BAL = "0xd3c21bcecceda1000000"
    addrs_lower = [a.lower() for a in addrs]

    # Read MN_CODE from existing genesis.json if it exists, otherwise use placeholder
    import os
    mn_code = "0x606060405260043610610196"  # truncated placeholder
    genesis_path = "/root/devnet-a97m/genesis.json"
    if os.path.exists(genesis_path):
        with open(genesis_path) as f:
            orig = json.load(f)
        mn_code = orig["alloc"]["0000000000000000000000000000000000000088"]["code"]
        print(f"[gen-genesis] Using MN_CODE from existing genesis ({len(mn_code)} chars)", file=sys.stderr)

    genesis = {
        "config": {
            "chainId": 551,
            "homesteadBlock": 0,
            "eip150Block": 0,
            "eip150Hash": "0x0000000000000000000000000000000000000000000000000000000000000000",
            "eip155Block": 0,
            "eip158Block": 0,
            "byzantiumBlock": 0,
            "eip1559Block": 999999,
            "cancunBlock": 999999,
            "XDPoS": {
                "period": 2,
                "epoch": 90,
                "reward": 10,
                "rewardCheckpoint": 90,
                "gap": 45,
                "foudationWalletAddr": "0x0000000000000000000000000000000000000068",
                "v2": {
                    "switchBlock": 0,
                    "switchEpoch": 0,
                    "config": {
                        "switchRound": 0,
                        "maxMasternodes": 108,
                        "minePeriod": 2,
                        "timeoutSyncThreshold": 3,
                        "timeoutPeriod": 10,
                        "certificateThreshold": 0.667,
                        "minimumMinerBlockPerEpoch": 1,
                        "limitPenaltyEpoch": 5,
                        "minimumSigningTx": 1
                    },
                    "allConfigs": {
                        "0": {
                            "switchRound": 0,
                            "maxMasternodes": 108,
                            "minePeriod": 2,
                            "timeoutSyncThreshold": 3,
                            "timeoutPeriod": 10,
                            "certificateThreshold": 0.667,
                            "minimumMinerBlockPerEpoch": 1,
                            "limitPenaltyEpoch": 5,
                            "minimumSigningTx": 1
                        }
                    }
                }
            }
        },
        "nonce": "0x0",
        "timestamp": "0x0",
        "extraData": extra_data,
        "gasLimit": "0x280de80",
        "difficulty": "0x1",
        "mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
        "coinbase": "0x0000000000000000000000000000000000000000",
        "alloc": {
            "0000000000000000000000000000000000000000": {"balance": "0x0"},
            "0000000000000000000000000000000000000068": {"balance": ALLOC_BAL},
            addrs_lower[0].replace('0x',''): {"balance": ALLOC_BAL},
            addrs_lower[1].replace('0x',''): {"balance": ALLOC_BAL},
            addrs_lower[2].replace('0x',''): {"balance": ALLOC_BAL},
            "0000000000000000000000000000000000000088": {
                "code": mn_code,
                "storage": storage,
                "balance": "0x0"
            }
        }
    }

    with open(out_file, 'w') as f:
        json.dump(genesis, f, indent=2)

    print(f"genesis.json written to {out_file}")
    print(f"Addresses: {', '.join(addrs_lower)}")
    print(f"SMC storage slots: {len(storage)}")

if __name__ == '__main__':
    main()
