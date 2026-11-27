package app

import "context"

// latest is a read whose newest run alone lands: Run drops the result of
// the run before it and cancels its context, Stop does the same without
// starting another. A screen keeps one per thing it reads instead of a
// counter; the zero value is ready. All its state lives on the UI thread.
//
// Cancelling matters because the Service serializes its reads on one
// database connection: a dropped read left to finish would hold it ahead
// of the read that replaced it. Over the Local API the cancel reaches the
// backend, which interrupts the statement.
type latest struct {
	loading bool // a run is in flight
	seq     uint64
	cancel  context.CancelFunc // the newest run's
	keep    bool               // the newest run came from RunKeep: a later Run leaves it to finish
}

// Run reads with fn off the UI thread, then calls done on the UI thread with
// fn's error, unless Run or Stop was called since: then done is dropped and
// fn's context is cancelled. fn must only read; work that must complete
// once started (a write) goes through RunKeep.
func (l *latest) Run(a *App, fn func(ctx context.Context) error, done func(err error)) {
	l.run(a, fn, done, false)
}

// RunKeep is Run for a fn whose work must complete once started: a later
// Run drops its done but leaves it to finish. Stop still cancels it.
func (l *latest) RunKeep(a *App, fn func(ctx context.Context) error, done func(err error)) {
	l.run(a, fn, done, true)
}

func (l *latest) run(a *App, fn func(ctx context.Context) error, done func(err error), keep bool) {
	if l.cancel != nil && !l.keep {
		l.cancel() // its done is dropped below: nothing waits for its result
	}
	l.seq++
	ctx, cancel := context.WithCancel(a.ctx)
	seq := l.seq
	l.loading, l.cancel, l.keep = true, cancel, keep
	a.do(func(context.Context) error { return fn(ctx) }, func(err error) {
		cancel()
		if seq != l.seq {
			return
		}
		l.loading, l.cancel, l.keep = false, nil, false
		done(err)
	})
}

// Also reads with fn as part of what the last Run read (the next page of
// it, say): its done is dropped by the next Run or Stop like the run's, but
// it neither shows as loading nor is cancelled.
func (l *latest) Also(a *App, fn func(ctx context.Context) error, done func(err error)) {
	seq := l.seq
	a.do(fn, func(err error) {
		if seq == l.seq {
			done(err)
		}
	})
}

// Stop drops the run in flight, if any, and cancels its context.
func (l *latest) Stop() {
	l.seq++
	if l.cancel != nil {
		l.cancel()
	}
	l.loading, l.cancel, l.keep = false, nil, false
}

// Gen numbers the runs: it rises with every Run and Stop, so a result
// stamped with it tells whether a later run has landed.
func (l *latest) Gen() uint64 { return l.seq }
