package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// "No money has gone missing" should be something the metric states, not
// something inferred from a series that does not exist. A CounterVec publishes
// no series until a label combination is first used, so before this the tier-1
// alert on kind="payment" had nothing to evaluate against until the first
// unattributed payment in the Hub's entire history.
//
// Order matters in this test and is the reason it works: GetMetricWithLabelValues
// CREATES the series as a side effect, so collecting has to happen first. An
// earlier version of this test asked for the values before collecting, which
// manufactured exactly the state it was meant to be checking and passed happily
// against a build with the initialisation removed.
func TestBothUnattributedKindsExistBeforeAnythingHappens(t *testing.T) {
	ch := make(chan prometheus.Metric, 16)
	PaymentsUnattributedTotal.Collect(ch)
	close(ch)

	kinds := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatalf("writing a collected metric: %v", err)
		}
		for _, l := range pb.GetLabel() {
			if l.GetName() == "kind" {
				kinds[l.GetValue()] = pb.GetCounter().GetValue()
			}
		}
	}

	for _, want := range []string{"payment", "lifecycle"} {
		v, ok := kinds[want]
		if !ok {
			t.Errorf("no series for kind=%q at rest. An alert on it has nothing to evaluate "+
				"until the first such event ever occurs, which for kind=payment means the "+
				"first time money goes missing.", want)
			continue
		}
		if v != 0 {
			t.Errorf("kind=%q reads %v at rest, want 0", want, v)
		}
	}
}
