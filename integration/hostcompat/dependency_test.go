package hostcompat

import (
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/s2"
)

func TestS2DictionaryRepeatBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		repeat uint64
		valid  bool
	}{
		{"zero", 0, true},
		{"last offset", s2.MinDictSize, true},
		{"past dictionary", s2.MinDictSize + 1, false},
		{"32-bit sign boundary", 1 << 31, false},
		{"64-bit sign boundary", 1 << 63, false},
		{"maximum unsigned value", ^uint64(0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := binary.AppendUvarint(nil, tc.repeat)
			raw = append(raw, make([]byte, s2.MinDictSize)...)
			// Validate only: never pass an invalid dictionary to the encoder.
			if got := s2.NewDict(raw); (got != nil) != tc.valid {
				t.Fatalf("NewDict repeat=%d: accepted=%v, want %v", tc.repeat, got != nil, tc.valid)
			}
		})
	}
}
