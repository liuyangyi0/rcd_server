// SPDX-License-Identifier: MIT
package db_lib

// AnsibleLaunchError is emitted only when Start failed: no playbook process
// existed. A Wait/SSH/remote failure MUST NOT be wrapped in this type.
type AnsibleLaunchError struct{ Err error }

func (e *AnsibleLaunchError) Error() string { return "Ansible 未启动: " + e.Err.Error() }
func (e *AnsibleLaunchError) Unwrap() error { return e.Err }
