package opc

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	gu "github.com/gopcua/opcua/ua"

	"rcd_manager/internal/api"
	"rcd_manager/internal/domain"
)

// TestLiveManagerMethodReportsSerialFailure is an opt-in real-lower test. The
// caller must make the selected physical port unavailable before running it;
// the test itself never changes acquisition configuration or device files.
func TestLiveManagerMethodReportsSerialFailure(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("RCD_COMMAND_FEEDBACK_ENDPOINT"))
	apiURL := strings.TrimRight(os.Getenv("RCD_COMMAND_FEEDBACK_API"), "/")
	methodID := strings.TrimSpace(os.Getenv("RCD_COMMAND_FEEDBACK_METHOD"))
	sendID := strings.TrimSpace(os.Getenv("RCD_COMMAND_FEEDBACK_SEND"))
	payload := strings.TrimSpace(os.Getenv("RCD_COMMAND_FEEDBACK_PAYLOAD"))
	if endpoint == "" || apiURL == "" || methodID == "" || sendID == "" || payload == "" {
		t.Skip("set RCD_COMMAND_FEEDBACK_ENDPOINT/API/METHOD/SEND/PAYLOAD for a real lower with an unavailable serial port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	clientName := "command-feedback-" + time.Now().UTC().Format("150405.000000000")
	client, _ := connectLiveManagerAnonymous(t, endpoint, clientName)
	defer client.Close(context.Background())
	session := liveDeviceFaultSession(t, ctx, apiURL, clientName)

	response, err := client.Call(ctx, &gu.CallMethodRequest{
		ObjectID:       gu.NewStringNodeID(2, "Methods"),
		MethodID:       gu.NewStringNodeID(2, methodID),
		InputArguments: []*gu.Variant{gu.MustVariant(payload)},
	})
	if err != nil || response == nil || response.StatusCode == gu.StatusOK || len(response.OutputArguments) != 0 {
		t.Fatalf("serial-unavailable Method response=%#v err=%v", response, err)
	}

	read, err := client.Read(ctx, &gu.ReadRequest{
		TimestampsToReturn: gu.TimestampsToReturnBoth,
		NodesToRead: []*gu.ReadValueID{{
			NodeID: gu.NewStringNodeID(2, sendID), AttributeID: gu.AttributeIDValue,
		}},
	})
	if err != nil || read == nil || len(read.Results) != 1 || read.Results[0] == nil ||
		read.Results[0].Status != response.StatusCode || read.Results[0].Value == nil || read.Results[0].Value.Value() != "3" {
		t.Fatalf("serial-unavailable _Send value=%#v err=%v", read, err)
	}

	var commands api.CommandPage
	liveGroupsJSON(t, apiURL, http.MethodGet, "/api/v1/commands?limit=20", 0, nil, &commands, http.StatusOK)
	for _, command := range commands.Items {
		if command.SessionID == session.ID && command.Kind == "Call" {
			if command.State != domain.CommandFailed || command.StatusCode != domain.StatusCode(response.StatusCode) || command.Error == "" {
				t.Fatalf("serial-unavailable command audit=%#v", command)
			}
			t.Logf("serial failure reached caller: Method=%s status=0x%08X, %s=3 status=0x%08X, command=%s state=%s",
				methodID, uint32(response.StatusCode), sendID, uint32(read.Results[0].Status), command.ID, command.State)
			return
		}
	}
	t.Fatalf("serial-unavailable Call audit not found for Session %s: %#v", session.ID, commands.Items)
}
