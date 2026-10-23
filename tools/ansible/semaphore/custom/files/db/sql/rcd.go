// SPDX-License-Identifier: MIT
package sql

import "github.com/semaphoreui/semaphore/services/rcd"

func (d *SqlDb) RCDRepository() *rcd.Repository {
	return &rcd.Repository{DB: d.Sql().Db, Bind: d.GetConnection().PrepareQuery}
}
