package conformance

import "sync/atomic"

// outcomeObserver, when set, is told of every outcome a result is built
// with (Result.As). It is a test seam: the package's tests set it to check
// that every declared outcome of every registered check is exercised. It is
// nil in a run.
var outcomeObserver atomic.Pointer[func(Outcome)]

// noteExercised reports o to the observer, if one is set.
func noteExercised(o Outcome) {
	if f := outcomeObserver.Load(); f != nil {
		(*f)(o)
	}
}
