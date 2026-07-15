package intercept

import "github.com/watchers-id/watchersid/pkg/filter"

type Settings struct {
	RequestsEnabled  bool
	ResponsesEnabled bool
	RequestFilter    filter.Expression
	ResponseFilter   filter.Expression
}
