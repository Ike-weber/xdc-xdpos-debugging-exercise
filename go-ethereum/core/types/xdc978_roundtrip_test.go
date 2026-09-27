package types

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"
)

// TestXDC978HeaderJSONRoundTrip guards issue #978: gen_header_json.go must
// (un)marshal the XDC Validators/Validator/Penalties fields so that a header
// round-tripped through JSON re-hashes to the same block hash. Before the fix
// these fields were dropped, zeroing them on decode and breaking Hash().
func TestXDC978HeaderJSONRoundTrip(t *testing.T) {
	h := &Header{
		Number:     big.NewInt(80369100),
		Difficulty: big.NewInt(105),
		GasLimit:   420000000,
		Validators: bytes.Repeat([]byte{0xab}, 420), // 21 * 20-byte addrs
		Validator:  bytes.Repeat([]byte{0xcd}, 65),  // M2 seal
		Penalties:  bytes.Repeat([]byte{0xef}, 40),
	}
	enc, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"validators":"0x`, `"validator":"0x`, `"penalties":"0x`} {
		if !bytes.Contains(enc, []byte(want)) {
			t.Fatalf("MarshalJSON dropped field %q; json=%s", want, enc)
		}
	}
	var got Header
	if err := json.Unmarshal(enc, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Validators, h.Validators) {
		t.Fatalf("Validators not preserved: got %x", got.Validators)
	}
	if !bytes.Equal(got.Validator, h.Validator) {
		t.Fatalf("Validator not preserved: got %x", got.Validator)
	}
	if !bytes.Equal(got.Penalties, h.Penalties) {
		t.Fatalf("Penalties not preserved: got %x", got.Penalties)
	}
	if got.Hash() != h.Hash() {
		t.Fatalf("block hash diverged after JSON round-trip: %s != %s", got.Hash(), h.Hash())
	}
}
