package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"rcd_manager/internal/domain"
)

func TestConsecutiveCommandResultsNotifyBeforePortRelease(t *testing.T) {
	for _, ack := range []string{"1", "3"} {
		t.Run(ack, func(t *testing.T) {
			r, _ := deviceRuntime(t)
			_ = r.openSession("sender")
			_ = r.openSession("observer")
			var values []domain.Sample
			r.SetCommandResultHandler(func(session string, key domain.NodeKey, sample domain.Sample) {
				if session != "sender" {
					t.Fatal("result leaked to another Session")
				}
				if r.routes.PendingCount("device-one") != 1 {
					t.Fatal("port unlocked before result was queued")
				}
				if key.Identifier == "ttya_Send" {
					values = append(values, sample)
				}
			})
			var previousID string
			for i := 0; i < 3; i++ {
				if status, err := r.WriteCommand(context.Background(), "sender", testKey("ttya_Send"), "01 06 00 01"); err != nil || !status.IsGood() {
					t.Fatalf("write %d: %v %v", i, status, err)
				}
				// Synchronous Methods still reject a busy port; asynchronous
				// writes are separately covered by the FIFO queue tests.
				if _, status, _ := r.CallCommand(context.Background(), "observer", testKey("ttya_SendCommand"), "01 06 00 01"); status != domain.StatusBadWaitingForResponse {
					t.Fatalf("busy=%x", status)
				}
				r.OnSample("a", testKey("ttya_Send"), domain.Sample{Type: domain.DataTypeString, Value: ack, StatusCode: domain.StatusGood, Epoch: 1})
				if r.routes.PendingCount("device-one") != 0 {
					t.Fatal("terminal command still pending")
				}
				result := r.ReadGroupCommandResult("sender", "device-one")
				var decoded struct {
					CommandID  string `json:"command_id"`
					StatusCode uint32 `json:"status_code"`
				}
				if !result.StatusCode.IsGood() || json.Unmarshal([]byte(result.Value.(string)), &decoded) != nil || decoded.CommandID == "" || decoded.CommandID == previousID {
					t.Fatalf("diagnostic=%+v", result)
				}
				if ack == "3" && decoded.StatusCode == 0 {
					t.Fatal("failure disguised as successful outcome")
				}
				previousID = decoded.CommandID
			}
			if len(values) != 3 {
				t.Fatalf("equal terminal notifications=%d", len(values))
			}
			for _, v := range values {
				if v.Value != ack {
					t.Fatalf("terminal=%+v", v)
				}
			}
			if r.ReadGroupCommandResult("observer", "device-one").StatusCode != domain.StatusBadWaitingInitialData {
				t.Fatal("other Session received result")
			}
		})
	}
}

func TestNoFeedbackTimeoutPublishesFailureAndAllowsNextCommand(t *testing.T) {
	r, _ := deviceRuntime(t)
	_ = r.openSession("sender")
	var notifications int
	r.SetCommandResultHandler(func(_ string, key domain.NodeKey, sample domain.Sample) {
		if key.Identifier == "ttya_Send" {
			notifications++
			if sample.Value != "3" || sample.StatusCode != domain.StatusBadTimeout {
				t.Fatalf("timeout=%+v", sample)
			}
		}
	})
	for i := 0; i < 3; i++ {
		if status, err := r.WriteCommand(context.Background(), "sender", testKey("ttya_Send"), "01 06 00 01"); err != nil || !status.IsGood() {
			t.Fatalf("write=%x %v", status, err)
		}
		r.sweepExpiredCommands(time.Now().Add(r.options.CommandPendingTTL + time.Second))
		if r.routes.PendingCount("device-one") != 0 {
			t.Fatal("timeout retained port")
		}
	}
	if notifications != 3 {
		t.Fatalf("notifications=%d", notifications)
	}
}

func TestTrackedAckTimeoutMapsToTimeout(t *testing.T) {
	r := seededRuntime(t)
	_ = r.openSession("s")
	lower := &trackingLower{fakeLowerClient: fakeLowerClient{writeStatus: domain.StatusGood}, supported: true}
	r.mu.Lock()
	installTestLower(r, "source-a", lowerHandle{client: lower, generation: 1, cancel: func() {}})
	r.statuses["source-a"] = connectedStatus("source-a", 1)
	r.mu.Unlock()
	primeWriteAck(r, "source-a", "COM1", 1)
	for i := 0; i < 3; i++ {
		if status, err := r.WriteCommand(context.Background(), "s", testKey("COM1_Send"), "1108"); err != nil || !status.IsGood() {
			t.Fatalf("write=%x %v", status, err)
		}
		lower.value.Phase, lower.value.Error = "Failed", "ack_timeout"
		r.acceptCommandEvidence("source-a", lower.value)
		if value := r.ReadSample("s", testKey("COM1_Send")); value.Value != "3" || value.StatusCode != domain.StatusBadTimeout {
			t.Fatalf("result=%+v", value)
		}
	}
}
