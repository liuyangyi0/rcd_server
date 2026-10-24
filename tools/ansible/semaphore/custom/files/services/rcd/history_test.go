// SPDX-License-Identifier: MIT
package rcd

import (
	"strings"
	"testing"
	"time"
)

func TestRCDInitialVersionRequiresSuccessfulLaterExactImageStart(t *testing.T) {
	hash := strings.Repeat("a", 64)
	installedAt, _ := time.Parse(time.RFC3339Nano, "2026-10-21T01:00:00Z")
	good := Operation{TaskID: 12, State: "succeeded", CreatedAt: "2026-10-21T01:00:01Z", Request: OperationRequest{Kind: "service_control", Component: "my-app", Action: "start"}, Targets: []ReleaseTarget{{DeviceID: "device", State: "succeeded", ExpectedBinarySHA256: hash, Acceptance: map[string]any{"state": "running", "binary_sha256": hash, "pid": float64(123)}}}}
	if !verifiedStartedImage(good, "device", "my-app", hash, installedAt) {
		t.Fatal("exact verified start rejected")
	}
	for _, change := range []func(*Operation){
		func(o *Operation) { o.State = "unknown" }, func(o *Operation) { o.State = "failed_before_execution" },
		func(o *Operation) { o.TaskID = 0 }, func(o *Operation) { o.Request.Action = "stop" },
		func(o *Operation) { o.Request.Component = "another-app" }, func(o *Operation) { o.CreatedAt = "2026-10-21T00:59:59Z" },
		func(o *Operation) { o.Targets[0].DeviceID = "other" }, func(o *Operation) { o.Targets[0].State = "unknown" },
		func(o *Operation) { o.Targets[0].ExpectedBinarySHA256 = strings.Repeat("b", 64) },
		func(o *Operation) { o.Targets[0].Acceptance["binary_sha256"] = strings.Repeat("b", 64) },
		func(o *Operation) { o.Targets[0].Acceptance["state"] = "stopped" },
		func(o *Operation) { o.Targets[0].Acceptance["pid"] = float64(0) },
		func(o *Operation) { delete(o.Targets[0].Acceptance, "pid") },
	} {
		copy := good
		copy.Targets = append([]ReleaseTarget{}, good.Targets...)
		copy.Targets[0].Acceptance = map[string]any{}
		for key, value := range good.Targets[0].Acceptance {
			copy.Targets[0].Acceptance[key] = value
		}
		change(&copy)
		if verifiedStartedImage(copy, "device", "my-app", hash, installedAt) {
			t.Fatalf("incomplete/late/failed proof accepted: %+v", copy)
		}
	}
}
