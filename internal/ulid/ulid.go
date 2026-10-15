// Package ulid makes ULIDs: 26-character IDs that sort by the time they
// were made (48-bit milliseconds, then 80 random bits), as GoToSocial uses.
package ulid

import (
	"crypto/rand"
	"strings"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// At returns a ULID (26 chars, lexically sortable by time) for t.
// Statuses use ULIDs derived from their publish time so that sorting by id
// sorts by time, like GoToSocial.
func At(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UnixMilli())
	if t.IsZero() || t.UnixMilli() < 0 {
		ms = 0
	}
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	rand.Read(b[6:])
	return Encode(b)
}

// Encode writes 128 bits as 26 base32 chars (first char holds 3 bits).
func Encode(b [16]byte) string {
	out := make([]byte, 26)
	var acc uint64
	bits := 0
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 && idx >= 0 {
			out[idx] = crockford[acc&31]
			acc >>= 5
			bits -= 5
			idx--
		}
	}
	if idx >= 0 {
		out[idx] = crockford[acc&31]
	}
	return string(out)
}

// New returns a ULID for the current time.
func New() string { return At(time.Now()) }

// Decode parses a ULID back into its 128 bits.
func Decode(id string) ([16]byte, bool) {
	var b [16]byte
	if len(id) != 26 {
		return b, false
	}
	var hi, lo uint64 // 128 bits, filled 5 at a time
	for _, c := range []byte(id) {
		v := strings.IndexByte(crockford, c)
		if v < 0 {
			return b, false
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	for i := 0; i < 8; i++ {
		b[i] = byte(hi >> (56 - 8*i))
		b[8+i] = byte(lo >> (56 - 8*i))
	}
	return b, true
}

// Number squeezes a ULID into a positive int64: its 48-bit millisecond
// time and the top 15 random bits. Some clients (Ice Cubes) want numbers
// where Mastodon has them, e.g. most_recent_notification_id.
func Number(id string) (int64, bool) {
	b, ok := Decode(id)
	if !ok {
		return 0, false
	}
	var ms uint64
	for i := 0; i < 6; i++ {
		ms = ms<<8 | uint64(b[i])
	}
	return int64(ms<<15 | uint64(b[6])<<7 | uint64(b[7])>>1), true
}

// FromNumber turns a number from Number back into the smallest (or, with
// upper, the largest) ULID that maps to it, for max_id/since_id paging.
func FromNumber(n int64, upper bool) string {
	var b [16]byte
	if upper {
		for i := 6; i < 16; i++ {
			b[i] = 0xff
		}
	}
	ms := uint64(n) >> 15
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	r := uint64(n) & 0x7fff
	b[6] = byte(r >> 7)
	b[7] = byte(r<<1) | b[7]&1
	return Encode(b)
}
