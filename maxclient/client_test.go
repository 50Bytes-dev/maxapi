package maxclient

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

func newTestClient() *Client {
	return NewClient("test-device", zerolog.Nop())
}

func TestNextSeqStartsAtOne(t *testing.T) {
	c := newTestClient()

	if got := c.nextSeq(); got != 1 {
		t.Fatalf("first seq = %d, want 1", got)
	}
	if got := c.nextSeq(); got != 2 {
		t.Fatalf("second seq = %d, want 2", got)
	}
}

// MAX parses seq as a signed 16-bit integer and silently drops the connection
// for anything above 32767, so the counter must never emit such a value.
func TestNextSeqWrapsAtMaxSeq(t *testing.T) {
	c := newTestClient()
	atomic.StoreInt32(&c.seq, MaxSeq-1)

	if got := c.nextSeq(); got != MaxSeq {
		t.Fatalf("seq before wrap = %d, want %d", got, MaxSeq)
	}
	if got := c.nextSeq(); got != 1 {
		t.Fatalf("seq after wrap = %d, want 1", got)
	}
}

func TestNextSeqStaysInRangeUnderConcurrency(t *testing.T) {
	c := newTestClient()
	atomic.StoreInt32(&c.seq, MaxSeq-50)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := c.nextSeq(); got < 1 || got > MaxSeq {
					t.Errorf("seq out of range: %d", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
