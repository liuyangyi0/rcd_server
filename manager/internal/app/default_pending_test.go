package app

import (
	"context"
	"errors"
	"testing"

	"rcd_manager/internal/domain"
)

func TestDefaultSourceWriteIsBlockedEvenWhenValueIsUnchanged(t *testing.T) {
	runtime, _ := deviceRuntime(t)
	ctx := context.Background()
	if err := runtime.openSession("initiator"); err != nil {
		t.Fatal(err)
	}
	before, err := runtime.GetGroup(ctx, "device-one")
	if err != nil {
		t.Fatal(err)
	}
	command, err := runtime.routes.BeginCommand("initiator", "device-one", "ttya", "Write")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"a", "b", ""} {
		if _, err := runtime.SetDefaultSource(ctx, "device-one", source, before.Revision); !errors.Is(err, domain.ErrGroupPending) {
			t.Fatalf("pending default write %q: %v", source, err)
		}
	}
	after, err := runtime.GetGroup(ctx, "device-one")
	if err != nil || after.Revision != before.Revision || after.DefaultSourceID != before.DefaultSourceID {
		t.Fatalf("rejected write changed configuration: before=%#v after=%#v err=%v", before, after, err)
	}
	if _, err := runtime.routes.FinishCommand(command.ID, domain.CommandReceived, domain.StatusGood, ""); err != nil {
		t.Fatal(err)
	}
	if after, err := runtime.SetDefaultSource(ctx, "device-one", "a", before.Revision); err != nil || after.Revision != before.Revision+1 {
		t.Fatalf("completed command did not unlock default control: %#v %v", after, err)
	}
}
