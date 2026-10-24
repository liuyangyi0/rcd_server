package domain

import (
	"encoding/json"
	"os"
	"testing"
)

func TestTrackedCommandGoldenContract(t *testing.T) {
	data, err := os.ReadFile("../../../contracts/command-tracking/v1/ack-received.json")
	if err != nil {
		t.Fatal(err)
	}
	var v TrackedCommand
	if err = json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if err = v.Validate(); err != nil {
		t.Fatal(err)
	}
	if v.AckID != "18446744073709551615" || v.SentCount != 2 || !v.Terminal() {
		t.Fatalf("%+v", v)
	}
}
