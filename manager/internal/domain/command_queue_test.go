package domain

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestHasPortCommandsIncludesUnclaimedQueueAndExecutingTransaction(t *testing.T) {
	table := testRouteTable(t)
	if _, err := table.OpenSession("s"); err != nil {
		t.Fatal(err)
	}
	if table.HasPortCommands("a", "COM1") {
		t.Fatal("empty physical port reported occupied")
	}
	first, err := table.EnqueueWrite("s", "g", "COM1", "COM1_Send", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := table.EnqueueWrite("s", "g", "COM1", "COM1_Send", "second")
	if err != nil {
		t.Fatal(err)
	}
	if !table.HasPortCommands("a", "COM1") || table.HasPortCommands("a", "COM2") || table.HasPortCommands("b", "COM1") {
		t.Fatal("queued command reservation is missing or leaked to another physical port")
	}
	if _, err := table.ActivateQueued(first.ID); err != nil {
		t.Fatal(err)
	}
	if !table.HasPortCommands("a", "COM1") {
		t.Fatal("executing command reservation is missing")
	}
	if _, err := table.CommitCommandFinish(first.ID); err != nil {
		t.Fatal(err)
	}
	if !table.HasPortCommands("a", "COM1") {
		t.Fatal("tail queue lost its reservation after the active command completed")
	}
	if _, err := table.CommitCommandFinish(second.ID); err != nil {
		t.Fatal(err)
	}
	if table.HasPortCommands("a", "COM1") {
		t.Fatal("completed physical port reservation leaked")
	}
}

func TestCommandQueueFIFOReservationsAndCapacity(t *testing.T) {
	table := testRouteTable(t)
	session, _ := table.OpenSession("s")
	var ids []string
	for i := 0; i < CommandQueueCapacity; i++ {
		c, err := table.EnqueueWrite("s", "g", "COM1", "COM1_Send", fmt.Sprint(i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if _, err := table.EnqueueWrite("s", "g", "COM1", "COM1_Send", "overflow"); !errors.Is(err, ErrCommandPending) {
		t.Fatal(err)
	}
	if _, err := table.Select("s", "g", "b", session.Routes["g"].Revision); !errors.Is(err, ErrGroupPending) {
		t.Fatal(err)
	}
	if _, err := table.BeginCommand("s", "g", "COM1", "Call"); !errors.Is(err, ErrCommandPending) {
		t.Fatal(err)
	}
	table.CloseSession("s")
	for i, id := range ids {
		head, ok := table.QueueHead("a", "COM1")
		if !ok || head.ID != id || head.Request != fmt.Sprint(i) {
			t.Fatalf("head=%+v", head)
		}
		if _, err := table.ActivateQueued(id); err != nil {
			t.Fatal(err)
		}
		if _, ok := table.QueueHead("a", "COM1"); ok {
			t.Fatal("active port exposed next head")
		}
		if _, err := table.CommitCommandFinish(id); err != nil {
			t.Fatal(err)
		}
	}
	if table.TotalPending() != 0 || table.PendingCount("g") != 0 {
		t.Fatal("reservation leaked")
	}
}

func TestCommandQueueCancelTailAtomicAndOtherPortsIndependent(t *testing.T) {
	table := testRouteTable(t)
	_, _ = table.OpenSession("s")
	first, _ := table.EnqueueWrite("s", "g", "COM1", "", "A")
	_, _ = table.ActivateQueued(first.ID)
	_, _ = table.EnqueueWrite("s", "g", "COM1", "", "B")
	other, _ := table.EnqueueWrite("s", "g", "COM2", "", "C")
	if _, err := table.ActivateQueued(other.ID); err != nil {
		t.Fatal(err)
	}
	_, cancelled, err := table.CommitCommandFinishWithQueue(first.ID, true)
	if err != nil || len(cancelled) != 1 || cancelled[0].Request != "B" || cancelled[0].State != CommandFailed {
		t.Fatalf("%v %+v", err, cancelled)
	}
	if table.TotalPending() != 1 {
		t.Fatal("unrelated port was released")
	}
}

func TestCommandQueueExpiryIncludesWaitingAndDoesNotReleaseActive(t *testing.T) {
	table := testRouteTable(t)
	_, _ = table.OpenSession("s")
	first, _ := table.EnqueueWrite("s", "g", "COM1", "", "A")
	_, _ = table.ActivateQueued(first.ID)
	second, _ := table.EnqueueWrite("s", "g", "COM1", "", "B")
	if len(table.ExpiredPending(time.Now().Add(time.Minute), time.Second)) != 2 {
		t.Fatal("queued age ignored")
	}
	if _, err := table.PrepareCommandFinish(second.ID, CommandTimedOut, StatusBadTimeout, "expired", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := table.CommitCommandFinish(second.ID); err != nil {
		t.Fatal(err)
	}
	if active, ok := table.PendingCommand("a", "COM1"); !ok || active.ID != first.ID {
		t.Fatal("queued finish released active")
	}
}

func TestPassiveResetReservesPortWithoutJoiningHardwareQueueOrExpiry(t *testing.T) {
	table := testRouteTable(t)
	_, _ = table.OpenSession("s")
	reset, err := table.BeginCommandWithDetails("s", "g", "COM1", CommandKindPassiveReset, "COM1_Send", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := table.EnqueueWrite("s", "g", "COM1", "COM1_Send", "command"); !errors.Is(err, ErrCommandPending) {
		t.Fatalf("hardware command appended to passive reset: %v", err)
	}
	if _, err := table.BeginCommand("s", "g", "COM2", "Call"); err != nil {
		t.Fatalf("unrelated physical port blocked: %v", err)
	}
	for _, expired := range table.ExpiredPending(time.Now().Add(time.Hour), time.Second) {
		if expired.ID == reset.ID {
			t.Fatal("passive reset became a hardware timeout candidate")
		}
	}
	table.SweepExpired(time.Now().Add(time.Hour), time.Second)
	if !table.HasPortCommands("a", "COM1") || table.TotalPending() != 1 {
		t.Fatal("hardware timeout scan released the in-flight passive RPC")
	}
	if _, err := table.CommitCommandFinish(reset.ID); err != nil || table.TotalPending() != 0 {
		t.Fatalf("passive reservation release: %v", err)
	}
}
