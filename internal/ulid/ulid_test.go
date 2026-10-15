package ulid

import (
	"testing"
	"time"
)

// Number squeezes a ULID into an int64 (for most_recent_notification_id),
// and FromNumber gives ULIDs that page from it as max_id/since_id.
func TestNumber(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := At(time.UnixMilli(int64(i) * 987654321))
		n, ok := Number(id)
		if !ok || n < 0 {
			t.Fatalf("%s -> %d %v", id, n, ok)
		}
		lo, hi := FromNumber(n, false), FromNumber(n, true)
		if !(lo <= id && id <= hi) {
			t.Fatalf("%s not within [%s, %s]", id, lo, hi)
		}
		if m, _ := Number(lo); m != n {
			t.Fatalf("bound %s maps to %d, not %d", lo, m, n)
		}
		if b, ok := Decode(id); !ok || Encode(b) != id {
			t.Fatalf("decode/encode %s", id)
		}
	}
	if _, ok := Number("not-a-ulid"); ok {
		t.Fatal("parsed garbage")
	}
}
