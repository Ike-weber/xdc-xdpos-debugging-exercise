// Hand-written RLP encoder/decoder for StateAccount.
// Replaces the auto-generated version to support balances > 2^256-1 (XDC).

package types

import (
	"io"
	"math/big"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

func (obj *StateAccount) EncodeRLP(_w io.Writer) error {
	w := rlp.NewEncoderBuffer(_w)
	_tmp0 := w.List()
	w.WriteUint64(obj.Nonce)

	// Use BigBalance (big.Int) for encoding when set (overflow > uint256)
	if obj.BigBalance != nil {
		w.WriteBigInt(obj.BigBalance)
	} else if obj.Balance == nil {
		w.Write(rlp.EmptyString)
	} else {
		w.WriteUint256(obj.Balance)
	}

	w.WriteBytes(obj.Root[:])
	w.WriteBytes(obj.CodeHash)
	w.ListEnd(_tmp0)
	return w.Flush()
}

// stateAccountBigBalDecode is a helper for decoding StateAccount with big.Int balance.
type stateAccountBigBalDecode struct {
	Nonce    uint64
	Balance  *big.Int
	Root     [32]byte
	CodeHash []byte
}

func (obj *StateAccount) DecodeRLP(s *rlp.Stream) error {
	// We decode with big.Int for balance to handle values > 2^256-1
	var dec stateAccountBigBalDecode
	if err := s.Decode(&dec); err != nil {
		return err
	}
	obj.Nonce = dec.Nonce
	obj.Root = dec.Root
	obj.CodeHash = dec.CodeHash

	if dec.Balance == nil {
		obj.Balance = new(uint256.Int)
		obj.BigBalance = nil
	} else {
		bal, overflow := uint256.FromBig(dec.Balance)
		if overflow {
			obj.Balance = bal // modular (wrapped) value
			obj.BigBalance = dec.Balance
		} else {
			obj.Balance = bal
			obj.BigBalance = nil
		}
	}
	return nil
}
