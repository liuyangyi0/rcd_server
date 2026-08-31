package app

import (
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestRuntimeForwardsValueAndQualityWithoutOrderingBySourceClock(t *testing.T) {
	for _, clock := range []string{"unchanged", "backwards"} {
		t.Run(clock, func(t *testing.T) {
			runtime := seededRuntime(t)
			runtime.OnLowerStatus(connectedStatus("source-a", 2))
			if err := runtime.openSession("session"); err != nil {
				t.Fatal(err)
			}
			initialTime := time.Now().UTC()
			currentTime := initialTime
			if clock == "backwards" {
				currentTime = initialTime.Add(-time.Hour)
			}
			runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{
				Type: domain.DataTypeDouble, Value: float64(11), StatusCode: domain.StatusGood, SourceTimestamp: initialTime, Epoch: 2,
			})
			for _, sample := range []domain.Sample{
				{Type: domain.DataTypeDouble, Value: float64(-1), StatusCode: domain.StatusBadOutOfService, SourceTimestamp: currentTime, Epoch: 2},
				{Type: domain.DataTypeDouble, Value: float64(101), StatusCode: domain.StatusGood, SourceTimestamp: currentTime, Epoch: 2},
			} {
				runtime.OnSample("source-a", testKey("KKS001"), sample)
				got := runtime.ReadSample("session", testKey("KKS001"))
				if got.Value != sample.Value || got.StatusCode != sample.StatusCode || !got.SourceTimestamp.Equal(sample.SourceTimestamp) || got.Epoch != sample.Epoch {
					t.Fatalf("current lower sample was lost: got=%+v want=%+v", got, sample)
				}
			}
			// A genuinely old connection must still be rejected even if its
			// source timestamp is later than every current-epoch observation.
			runtime.OnSample("source-a", testKey("KKS001"), domain.Sample{
				Type: domain.DataTypeDouble, Value: float64(999), StatusCode: domain.StatusGood, SourceTimestamp: initialTime.Add(time.Hour), Epoch: 1,
			})
			got := runtime.ReadSample("session", testKey("KKS001"))
			if got.Value != float64(101) || got.Epoch != 2 || !got.SourceTimestamp.Equal(currentTime) {
				t.Fatalf("old epoch replaced current data: %+v", got)
			}
		})
	}
}
