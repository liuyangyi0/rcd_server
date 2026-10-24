package domain

import (
	"testing"
	"time"
)

func TestResolveStationCommunication(t *testing.T) {
	now := time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		edit func(*StationVersionPort, *StationVersion, *bool, *bool, *time.Time)
		want uint32
	}{
		{"normal", func(*StationVersionPort, *StationVersion, *bool, *bool, *time.Time) {}, 1},
		{"failed", func(_ *StationVersionPort, d *StationVersion, _, _ *bool, _ *time.Time) { d.CommunicationOK = false }, 2},
		{"port closed", func(p *StationVersionPort, _ *StationVersion, _, _ *bool, _ *time.Time) { p.Open = false }, 3},
		{"port disabled", func(p *StationVersionPort, _ *StationVersion, _, _ *bool, _ *time.Time) {
			p.Enabled, p.Open = false, false
		}, 4},
		{"station disabled", func(_ *StationVersionPort, d *StationVersion, _, _ *bool, _ *time.Time) { d.ReadEnabled = false }, 4},
		{"offline overrides disabled", func(p *StationVersionPort, _ *StationVersion, online, _ *bool, _ *time.Time) {
			p.Enabled, *online = false, false
		}, 5},
		{"stale", func(_ *StationVersionPort, _ *StationVersion, _, _ *bool, observed *time.Time) {
			*observed = now.Add(-StationCommunicationMaxAge - time.Millisecond)
		}, 0},
		{"not observed", func(_ *StationVersionPort, _ *StationVersion, _, _ *bool, observed *time.Time) {
			*observed = time.Time{}
		}, 0},
		{"cached or wrong epoch", func(_ *StationVersionPort, _ *StationVersion, _, fresh *bool, _ *time.Time) { *fresh = false }, 0},
		{"clock moved backwards", func(_ *StationVersionPort, _ *StationVersion, _, _ *bool, observed *time.Time) {
			*observed = now.Add(time.Second)
		}, 0},
		{"pending control", func(p *StationVersionPort, _ *StationVersion, _, _ *bool, _ *time.Time) { p.ControlPending = true }, 0},
		{"not applied", func(_ *StationVersionPort, d *StationVersion, _, _ *bool, _ *time.Time) { d.AppliedReadEnabled = false }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := StationVersionPort{Enabled: true, AppliedEnabled: true, Open: true}
			d := StationVersion{ReadEnabled: true, AppliedReadEnabled: true, CommunicationOK: true}
			online, fresh, observed := true, true, now
			tc.edit(&p, &d, &online, &fresh, &observed)
			got := ResolveStationCommunication(online, fresh, observed, now, p, d)
			if got.State != tc.want || got.Text() == "" || got.ObservedAt != observed {
				t.Fatalf("result = %#v, want state %d", got, tc.want)
			}
		})
	}
}
