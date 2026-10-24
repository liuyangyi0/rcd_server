package app

import (
	"context"
	"rcd_manager/internal/api"
)

func (r *Runtime) IngestLowerLogs(ctx context.Context, batch api.LowerLogBatch) (api.LowerLogAck, error) {
	return r.lowerLogs.IngestLowerLogs(ctx, batch)
}
func (r *Runtime) ListLowerLogs(ctx context.Context, request api.LowerLogRequest) (api.LowerLogPage, error) {
	return r.lowerLogs.ListLowerLogs(ctx, request)
}

var _ api.LowerLogsService = (*Runtime)(nil)
