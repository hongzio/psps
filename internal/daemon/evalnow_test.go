package daemon

import (
	"testing"
	"time"

	"github.com/hongzio/psps/internal/store"
)

func TestAnEvalUsuallyTakesTheMedianOfTheLastFiveThatRan(t *testing.T) {
	m := func(n ...int) []store.Eval {
		var out []store.Eval
		for _, s := range n {
			out = append(out, store.Eval{Took: time.Duration(s) * time.Second, NotRun: s < 0})
		}
		return out
	}
	for _, c := range []struct {
		evals []store.Eval
		want  time.Duration
	}{
		{nil, 0},
		{m(100), 100 * time.Second},
		{m(1440, 1440, 340, 480, 390), 480 * time.Second},
		{m(1440, 340, 480, -1, 390, 600), 480 * time.Second},
	} {
		if got := usualTook(c.evals); got != c.want {
			t.Errorf("%v: %s, not %s", c.evals, got, c.want)
		}
	}
}
