// Package store provides the PostgreSQL-backed persistence layer for the
// control plane.
package store

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// crockford is the Crockford base32 alphabet used by the ULID text format:
// no I, L, O, or U, so a generated ID is unambiguous when read aloud or
// transcribed by hand.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var ulidState struct {
	mu       sync.Mutex
	lastMS   int64
	lastRand [10]byte
}

// NewULID returns a new 26-character, lexically-sortable primary key: a
// Crockford base32 encoding of a 48-bit Unix millisecond timestamp followed
// by 80 bits of randomness (the ULID layout, https://github.com/ulid/spec).
//
// Every operational table in migrations/ uses application-generated text
// ULIDs as its primary key (see docs/initial-design/culture-nodes-prd-spec.md
// §14) rather than database-generated identifiers, so IDs are known before
// insert and sort by creation time without a separate timestamp column.
//
// Within the same millisecond, the random component is incremented as a
// big-endian counter rather than re-randomized, so IDs generated in a tight
// loop (e.g. inserting many ledger records in one transaction) still sort in
// generation order.
func NewULID() string {
	ulidState.mu.Lock()
	defer ulidState.mu.Unlock()

	ms := time.Now().UnixMilli()

	var randPart [10]byte
	if ms == ulidState.lastMS {
		randPart = ulidState.lastRand
		incrementCounter(&randPart)
	} else {
		if _, err := rand.Read(randPart[:]); err != nil {
			// crypto/rand.Read only fails if the OS entropy source is
			// unavailable, which makes the process environment unusable
			// well beyond ID generation.
			panic(fmt.Sprintf("store: crypto/rand unavailable: %v", err))
		}
	}
	ulidState.lastMS = ms
	ulidState.lastRand = randPart

	var payload [16]byte
	payload[0] = byte(ms >> 40)
	payload[1] = byte(ms >> 32)
	payload[2] = byte(ms >> 24)
	payload[3] = byte(ms >> 16)
	payload[4] = byte(ms >> 8)
	payload[5] = byte(ms)
	copy(payload[6:], randPart[:])

	return encodeULID(payload)
}

// incrementCounter increments b as a big-endian integer, in place, carrying
// across bytes. It is used to keep same-millisecond ULIDs monotonic.
func incrementCounter(b *[10]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

// encodeULID renders a 128-bit payload as 26 Crockford base32 characters.
//
// 26 characters * 5 bits = 130 bits, two more than the 128-bit payload, so
// the encoding is defined over a virtual 130-bit sequence formed by two
// leading zero bits followed by the payload's bits, read most-significant
// bit first. That makes the first character's value always fall in 0-7 (it
// only ever carries 3 real payload bits) while every other character
// carries a full 5 bits, and it keeps the text form ordered exactly like
// the underlying integer.
func encodeULID(payload [16]byte) string {
	var out [26]byte
	for i := range out {
		var v byte
		for b := 0; b < 5; b++ {
			bitPos := i*5 + b - 2 // -2 for the two virtual leading zero bits
			var bit byte
			if bitPos >= 0 {
				byteIdx := bitPos / 8
				shift := 7 - uint(bitPos%8)
				bit = (payload[byteIdx] >> shift) & 1
			}
			v = (v << 1) | bit
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
