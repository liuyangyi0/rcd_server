package domain

import (
	"errors"
	"fmt"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrAlreadyExists       = errors.New("already exists")
	ErrRevisionConflict    = errors.New("revision conflict")
	ErrGroupPending        = errors.New("group has a pending command")
	ErrCommandPending      = errors.New("source port already has a pending command")
	ErrSourceNotInGroup    = errors.New("source does not belong to group")
	ErrSourceNotSelectable = errors.New("source catalog is not selectable")
	ErrGroupInUse          = errors.New("group is used by an active session")
	ErrSourceInUse         = errors.New("source is configured or used by an active session")
	ErrSessionClosed       = errors.New("session is not active")
	ErrNodeRetired         = errors.New("node is retired")
	ErrNodeNotConfigured   = errors.New("node is not configured for selected source")
	ErrShuttingDown        = errors.New("manager is shutting down")
	ErrInvalid             = errors.New("invalid input")
)

type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func (e *FieldError) Unwrap() error { return ErrInvalid }

type CrossGroupDuplicateError struct {
	Identifier string
	FirstGroup string
	OtherGroup string
}

func (e *CrossGroupDuplicateError) Error() string {
	return fmt.Sprintf("node %q appears in groups %q and %q", e.Identifier, e.FirstGroup, e.OtherGroup)
}

func (e *CrossGroupDuplicateError) Unwrap() error { return ErrInvalid }

type RevisionConflictError struct {
	Expected uint64
	Actual   uint64
}

func (e *RevisionConflictError) Error() string {
	return fmt.Sprintf("revision conflict: expected %d, actual %d", e.Expected, e.Actual)
}

func (e *RevisionConflictError) Unwrap() error { return ErrRevisionConflict }
