package app

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestMaintenanceMutationLeaseCannotReleaseItsSuccessor(t *testing.T) {
	var service maintenanceService
	first, err := service.beginMutation("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.beginMutation("a"); err == nil {
		t.Fatal("overlapping mutation")
	}
	if err := service.beginProcess("a", "restart"); err == nil {
		t.Fatal("restart bypassed mutation")
	}
	if _, err := service.beginMutation("b"); err != nil {
		t.Fatal("unrelated source blocked", err)
	}
	service.endMutation("a", first)
	second, err := service.beginMutation("a")
	if err != nil {
		t.Fatal(err)
	}
	service.endMutation("a", first)
	if !service.maintained("a") {
		t.Fatal("late completion released successor")
	}
	service.endMutation("a", second)
	if service.maintained("a") {
		t.Fatal("lease leaked")
	}
}

func TestMaintenanceUnknownProcessFenceOutlivesExecution(t *testing.T) {
	var service maintenanceService
	if err := service.beginProcess("a", "restart"); err != nil {
		t.Fatal(err)
	}
	if service.tryReconcile("a", "restart") {
		t.Fatal("overlapping RPC")
	}
	service.endExecution("a", "restart")
	if service.check("a", true) == nil {
		t.Fatal("unknown outcome lost its fence")
	}
	if _, err := service.beginMutation("a"); err == nil {
		t.Fatal("mutation bypassed unknown outcome")
	}
	if service.tryReconcile("a", "different") {
		t.Fatal("wrong request reconciled")
	}
	if !service.tryReconcile("a", "restart") {
		t.Fatal("original query blocked")
	}
	service.releaseFence("a", "old-request")
	if service.pending("a") != "restart" {
		t.Fatal("wrong completion cleared fence")
	}
	service.releaseFence("a", "restart")
	if !service.maintained("a") {
		t.Fatal("execution lease still active")
	}
	service.endExecution("a", "restart")
	if service.maintained("a") {
		t.Fatal("terminal lease leaked")
	}
}

func TestMaintenanceSnapshotAndRecoveryAreIsolated(t *testing.T) {
	var service maintenanceService
	if err := service.restoreFence("a", "original"); err != nil {
		t.Fatal(err)
	}
	if err := service.restoreFence("a", "original"); err != nil {
		t.Fatal(err)
	}
	if err := service.restoreFence("a", "other"); err == nil {
		t.Fatal("conflicting durable intent overwritten")
	}
	snapshot := service.pendingSnapshot()
	snapshot["a"] = "changed"
	if service.pending("a") != "original" {
		t.Fatal("mutable snapshot exposed component state")
	}
}

func TestMaintenanceConcurrentAdmissionHasOneWinner(t *testing.T) {
	var service maintenanceService
	var count atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.beginProcess("a", "restart"); err == nil {
				count.Add(1)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("accepted %d executions", count.Load())
	}
}
