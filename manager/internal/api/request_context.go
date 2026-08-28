package api

import "context"

type RequestInfo struct {
	Actor         string
	RemoteAddress string
	UserAgent     string
}

type requestInfoContextKey struct{}

func withRequestInfo(ctx context.Context, info RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoContextKey{}, info)
}

func RequestInfoFromContext(ctx context.Context) RequestInfo {
	info, _ := ctx.Value(requestInfoContextKey{}).(RequestInfo)
	return info
}
