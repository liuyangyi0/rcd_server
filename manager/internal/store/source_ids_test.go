package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"rcd_manager/internal/calculation"
	"rcd_manager/internal/domain"
)

const migrationConfig = `[[state_inputs]]
name="COMM"
mode="group_default"
group_id="real-hardware"
[[state_inputs.targets]]
source_id="rcd92-46"
port_id="/dev/ttyWCH0"
device_id="11"
[[rules]]
kks_calc="real-lower_KEEP"
data_type="UInt32"
expression="COMM"
`

func migrationRepository(t *testing.T) *Repository {
	t.Helper()
	r := openTestRepository(t)
	ctx := context.Background()
	if _, err := r.CreateGroup(ctx, domain.Group{ID: "real-hardware", Name: "真实串口设备"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range legacyMachineSources {
		if _, err := r.CreateSource(ctx, domain.Source{ID: v.old, Name: "RCD", GroupID: "real-hardware", Endpoint: v.endpoint, SecurityPolicy: "None", SecurityMode: "None", Enabled: true}, "credential-unchanged"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.db.ExecContext(ctx, `UPDATE groups SET default_source_id='real-lower'`); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNormalizeSourceIDsAtomicReferencesAndHistory(t *testing.T) {
	r := migrationRepository(t)
	ctx := context.Background()
	before, err := calculation.ParseConfig(migrationConfig)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := r.SaveCalculationDraft(ctx, calculation.Draft{Content: migrationConfig, Hash: before.Hash}, 1)
	if err != nil {
		t.Fatal(err)
	}
	version, err := r.ActivateCalculation(ctx, before, "original", "active", calculation.DiffSummary{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.db.ExecContext(ctx, `INSERT INTO process_bindings VALUES('real-lower','http://192.168.10.46:8090','real-lower',1,?)`, encodeTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	policy := domain.DefaultRedundancyPolicy("real-hardware")
	policy.Priority = []string{"real-lower", "rcd92-58"}
	policy.LeftSourceID = "real-lower"
	policy.RightSourceID = "rcd92-58"
	if _, err = r.SaveRedundancy(ctx, policy, 1); err != nil {
		t.Fatal(err)
	}
	if err = r.SaveCommand(ctx, domain.PendingCommand{ID: "history", SourceID: "real-lower", GroupID: "real-hardware", State: domain.CommandFailed, StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	result, err := r.NormalizeLegacySourceIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sources) != 3 || result.Groups["real-hardware"] != "serial-acquisition" || !result.DraftChanged || result.CalculationVersions != 1 {
		t.Fatal(result)
	}
	for _, v := range legacyMachineSources {
		s, err := r.GetSource(ctx, v.id)
		if err != nil || s.GroupID != "serial-acquisition" || s.Endpoint != v.endpoint {
			t.Fatal(s, err)
		}
		_, password, err := r.SourceCredential(ctx, v.id)
		if err != nil || password != "credential-unchanged" {
			t.Fatal("credential changed", err)
		}
		if _, err = r.GetSource(ctx, v.old); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("legacy ID still accepted", err)
		}
	}
	g, err := r.GetGroup(ctx, "serial-acquisition")
	if err != nil || g.DefaultSourceID != "rcd-46" || g.Name != "串口采集组" {
		t.Fatal(g, err)
	}
	binding, err := r.GetProcessBinding(ctx, "rcd-46")
	if err != nil || binding.InstanceID != "real-lower" || binding.Revision != 1 {
		t.Fatal("Agent identity changed", binding, err)
	}
	active, err := r.LoadActiveCalculation(ctx)
	if err != nil || active.Revision == version.Revision || strings.Contains(active.Content, `"rcd92-46"`) {
		t.Fatal(active, err)
	}
	after, err := calculation.ParseConfig(active.Content)
	if err != nil || !reflect.DeepEqual(before.Rules, after.Rules) {
		t.Fatal("business rule altered", err)
	}
	if after.StateInputs[0].Targets[0].SourceID != "rcd-46" || after.StateInputs[0].GroupID != "serial-acquisition" {
		t.Fatal(after)
	}
	old, err := r.LoadCalculationVersion(ctx, version.ID)
	if err != nil || old.Content != migrationConfig {
		t.Fatal("immutable history rewritten", err)
	}
	updated, err := r.LoadCalculationDraft(ctx)
	if err != nil || updated.Revision != draft.Revision+1 || updated.Valid {
		t.Fatal(updated, err)
	}
	commands, _, err := r.ListCommands(ctx, 10, 0)
	if err != nil || commands[0].SourceID != "real-lower" || commands[0].GroupID != "real-hardware" {
		t.Fatal("audit history altered", commands, err)
	}
	policies, err := r.LoadRedundancy(ctx)
	if err != nil || policies[0].Priority[0] != "rcd-46" || policies[0].GroupID != "serial-acquisition" {
		t.Fatal(policies, err)
	}
	var table, parent string
	var rowid, key int
	if err = r.db.QueryRowContext(ctx, `PRAGMA foreign_key_check`).Scan(&table, &rowid, &parent, &key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign key check", table, rowid, parent, key, err)
	}
	// No-op replay cannot increment a revision or create another active version.
	again, err := r.NormalizeLegacySourceIDs(ctx)
	if err != nil || len(again.Sources) != 0 || again.DraftChanged || again.CalculationVersions != 0 {
		t.Fatal(again, err)
	}
	current, _ := r.LoadActiveCalculation(ctx)
	if current.Revision != active.Revision {
		t.Fatal("migration replay changed version")
	}
}

func TestNormalizeSourceIDsRejectsConflictsAndRollsBack(t *testing.T) {
	for _, test := range []string{"source_collision", "group_collision", "endpoint", "pending", "invalid_draft"} {
		t.Run(test, func(t *testing.T) {
			r := migrationRepository(t)
			ctx := context.Background()
			switch test {
			case "source_collision":
				_, _ = r.CreateSource(ctx, domain.Source{ID: "rcd-30", Name: "other", GroupID: "real-hardware", Endpoint: "opc.tcp://192.168.10.99:4841", SecurityPolicy: "None", SecurityMode: "None"}, "")
			case "group_collision":
				_, _ = r.CreateGroup(ctx, domain.Group{ID: "serial-acquisition", Name: "other"})
			case "endpoint":
				_, _ = r.db.ExecContext(ctx, `UPDATE sources SET endpoint='opc.tcp://192.168.10.99:4841' WHERE id='real-lower'`)
			case "pending":
				_ = r.SaveCommand(ctx, domain.PendingCommand{ID: "pending", SourceID: "real-lower", GroupID: "real-hardware", State: domain.CommandWaiting, StartedAt: time.Now()})
			case "invalid_draft":
				_, _ = r.db.ExecContext(ctx, `UPDATE calculation_draft SET content='[invalid'`)
			}
			if _, err := r.NormalizeLegacySourceIDs(ctx); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			if _, err := r.GetSource(ctx, "real-lower"); err != nil {
				t.Fatal("failed transaction changed ID", err)
			}
			if _, err := r.GetGroup(ctx, "real-hardware"); err != nil {
				t.Fatal("failed transaction changed group", err)
			}
		})
	}
}
