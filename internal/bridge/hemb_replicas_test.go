package bridge

import (
	"sync"
	"testing"

	"github.com/meshsat/meshsat-hub/internal/protocol"
)

// alwaysDecodes stands in for the reassembly buffer: every symbol completes
// a generation, which is what a K=1 generation does for real.
type alwaysDecodes struct{}

func (alwaysDecodes) AddSymbol(_ uint8, _ uint8, sym protocol.HeMBCodedSymbol) ([]byte, error) {
	return sym.Data, nil
}
func (alwaysDecodes) Reap() int                           { return 0 }
func (alwaysDecodes) Stats() protocol.HeMBReassemblyStats { return protocol.HeMBReassemblyStats{} }

type countingBus struct {
	mockBus
	mu        sync.Mutex
	published map[string]int
}

func (c *countingBus) Publish(topic string, _ byte, _ bool, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.published == nil {
		c.published = map[string]int{}
	}
	c.published[topic]++
	return nil
}

func hembFrame(gen uint16) []byte {
	hdr := protocol.MarshalHeMBExtended(protocol.HeMBExtendedHeader{K: 1, N: 1, GenerationID: gen, TotalPayloadSize: 3})
	frame := append([]byte{}, hdr[:]...)
	frame = append(frame, 1)             // one coefficient: identity
	frame = append(frame, 'h', 'e', 'y') // the payload
	return frame
}

// The reassembly buffer is in-memory and per process, so both replicas decode
// every generation; before MESHSAT-1120 both then published it and every
// mo/decoded consumer saw the message twice. One replica publishes now.
func TestHeMBGenerationIsPublishedByOneReplica(t *testing.T) {
	shared := newMockStore()
	mk := func() (*Subscriber, *countingBus) {
		cb := &countingBus{mockBus: *newMockBus()}
		s := NewSubscriber(cb, shared, nil)
		s.SetHeMBReassembler(alwaysDecodes{})
		return s, cb
	}
	a, busA := mk()
	b, busB := mk()

	topic := "meshsat/bridge/kit-1/hemb"
	a.handleHeMBSymbol(topic, hembFrame(7))
	b.handleHeMBSymbol(topic, hembFrame(7))
	total := busA.published[protocol.TopicDeviceMessage("kit-1")] + busB.published[protocol.TopicDeviceMessage("kit-1")]
	if total != 1 {
		t.Fatalf("generation 7 was published %d times across two replicas, want exactly 1", total)
	}

	// The next generation is a new claim and is published again.
	a.handleHeMBSymbol(topic, hembFrame(8))
	b.handleHeMBSymbol(topic, hembFrame(8))
	total = busA.published[protocol.TopicDeviceMessage("kit-1")] + busB.published[protocol.TopicDeviceMessage("kit-1")]
	if total != 2 {
		t.Fatalf("two generations were published %d times in total, want 2", total)
	}
}
