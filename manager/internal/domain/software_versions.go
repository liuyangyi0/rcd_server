package domain

import (
	"fmt"
	"rcd_manager/internal/buildinfo"
	"strings"
	"time"
	"unicode"
)

// Only software identity is persisted; never process control permission or passwords.
type SoftwareVersion struct {
	SourceID   string          `json:"source_id,omitempty"`
	Name       string          `json:"name"`
	Component  string          `json:"component"`
	Endpoint   string          `json:"endpoint"`
	Build      *buildinfo.Info `json:"build,omitempty"`
	BootID     string          `json:"boot_id,omitempty"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	ObservedAt time.Time       `json:"observed_at,omitempty,omitzero"`
	CheckedAt  time.Time       `json:"checked_at,omitempty,omitzero"`
	State      string          `json:"state"`
	Cached     bool            `json:"cached"`
	Message    string          `json:"message,omitempty"`
}

func ValidateSoftwareBuild(info buildinfo.Info, component string) error {
	if info.SchemaVersion != 1 || info.Component != component || strings.TrimSpace(info.Version) == "" {
		return fmt.Errorf("invalid software identity")
	}
	for _, value := range []string{info.Version, info.Commit, info.BuiltAt, info.ReleaseID, info.SourceSHA256, info.Target} {
		if len(value) > 256 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("invalid software build field")
		}
	}
	return nil
}
