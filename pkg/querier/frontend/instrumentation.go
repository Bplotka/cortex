package frontend

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	instr "github.com/weaveworks/common/instrument"
)

func instrument(name string, queryRangeDuration *prometheus.HistogramVec) QueryRangeMiddleware {
	return queryRangeMiddlewareFunc(func(next QueryRangeHandler) QueryRangeHandler {
		return QueryRangeHandlerFunc(func(ctx context.Context, req *QueryRangeRequest) (*APIResponse, error) {
			var resp *APIResponse
			err := instr.TimeRequestHistogram(ctx, name, queryRangeDuration, func(ctx context.Context) error {
				var err error
				resp, err = next.Do(ctx, req)
				return err
			})
			return resp, err
		})
	})
}
