// Package verifier: the wait loop (`verify --wait`).
//
// Without a wait time, Verify looks at the cluster one time. Run directly
// after `apply --dry-run=false`, that look finds a rollout in progress: old
// pods with no class, or no pods at all. The result is a mismatch (exit 4)
// for a migration that is correct and not finished. A pipeline cannot use
// that as a gate.
//
// With a wait time, Verify looks again every PollInterval until every step
// is ok or the time ends. Three rules make the result one that a pipeline
// can act on:
//
//  1. Pods must be Ready (readiness.go), and stay Ready. One look that
//     finds a pod Ready is not enough. A container with no readiness probe
//     is Ready from its start until it stops, so a pod in a crash loop is
//     Ready for a moment in each restart. A step is ok only when the same
//     pods, with the same restart counts, were Ready in looks that are at
//     least Settle apart.
//
//  2. An ok step stays ok. The loop does not look at it again. A step
//     that is not ok is looked at in every round.
//
//  3. The report holds the last complete round. The wait time is checked
//     between rounds, never by a deadline on the API calls. A round that a
//     deadline cut in the middle would replace "pod web-0 is not Ready:
//     CrashLoopBackOff" with "context deadline exceeded", which tells the
//     operator nothing. If the caller cancels in the middle of a round,
//     that round is dropped and the one before it is reported.
package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
)

const (
	// DefaultPollInterval is the time between two looks at the cluster.
	DefaultPollInterval = 5 * time.Second

	// DefaultSettle is how long the pods of a step must stay Ready, with
	// no restart and no replaced pod, before the step is ok. It is never
	// more than the wait time: `--wait 5s` on a cluster that is already
	// in the wanted state must be able to pass.
	DefaultSettle = 10 * time.Second
)

// Clock is the time source of the wait loop. Tests give a clock that moves
// only when the loop sleeps, so a three-minute wait runs in no time and
// each round is at a known instant.
type Clock interface {
	Now() time.Time
	// Sleep returns after d, or sooner when ctx is done.
	Sleep(ctx context.Context, d time.Duration)
}

// systemClock is the Clock of production.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// waitConfig is Options, with the defaults filled.
type waitConfig struct {
	wait     time.Duration
	interval time.Duration
	settle   time.Duration
	clock    Clock
}

func newWaitConfig(opts Options) waitConfig {
	cfg := waitConfig{wait: opts.Wait, interval: opts.PollInterval, settle: opts.Settle, clock: opts.Clock}
	if cfg.interval <= 0 {
		cfg.interval = DefaultPollInterval
	}
	if cfg.settle <= 0 {
		cfg.settle = DefaultSettle
	}
	if cfg.settle > cfg.wait {
		cfg.settle = cfg.wait
	}
	if cfg.clock == nil {
		cfg.clock = systemClock{}
	}
	return cfg
}

// stability is what the loop remembers about one step between rounds: the
// pods that were Ready (as a fingerprint), and since when.
type stability struct {
	fingerprint string
	since       time.Time

	// probed is the result of the in-pod probe for this set of pods, when
	// the probe gave an answer. The answer cannot change while the pods
	// are the same, so the loop does not exec into them again.
	probed *probeOutcome
}

// probeOutcome is the part of a VerifyResult that the in-pod probe sets.
type probeOutcome struct {
	status  schema.VerifyStatus
	message string
	probe   *schema.ProbeResult
}

// waitState is the state of all steps after a complete round.
type waitState struct {
	results []schema.VerifyResult
	stable  []stability
	final   []bool // true: the step is ok, and the loop does not look again
}

func (s waitState) clone() waitState {
	c := waitState{
		results: make([]schema.VerifyResult, len(s.results)),
		stable:  make([]stability, len(s.stable)),
		final:   make([]bool, len(s.final)),
	}
	copy(c.results, s.results)
	copy(c.stable, s.stable)
	copy(c.final, s.final)
	return c
}

func (s waitState) finalCount() int {
	n := 0
	for _, f := range s.final {
		if f {
			n++
		}
	}
	return n
}

// waitForSteps is the loop. It returns the results of the last complete
// round and a record of the wait. The error is not nil only when the caller
// canceled before the first round was complete: then there is no result to
// report.
func waitForSteps(ctx context.Context, deps stepDeps, steps []schema.PlanStep, cfg waitConfig) ([]schema.VerifyResult, *schema.VerifyWait, error) {
	start := cfg.clock.Now()
	deadline := start.Add(cfg.wait)
	info := &schema.VerifyWait{RequestedSeconds: wholeSeconds(cfg.wait)}
	state := waitState{
		results: make([]schema.VerifyResult, len(steps)),
		stable:  make([]stability, len(steps)),
		final:   make([]bool, len(steps)),
	}
	reported := -1 // the ok count in the last progress line

	for {
		now := cfg.clock.Now()
		next := lookRound(ctx, deps, steps, state, now, cfg.settle)
		if err := ctx.Err(); err != nil {
			// The API calls of this round failed with the context
			// error, or would have. Drop the round.
			if info.Rounds == 0 {
				return nil, nil, fmt.Errorf("verifier: canceled before the first look at the cluster was complete: %w", err)
			}
			info.Interrupted = true
			return state.results, info, nil
		}
		state = next
		info.Rounds++
		info.WaitedSeconds = wholeSeconds(now.Sub(start))

		ok := state.finalCount()
		if ok == len(steps) {
			return state.results, info, nil
		}
		remaining := deadline.Sub(cfg.clock.Now())
		if remaining <= 0 {
			info.TimedOut = true
			return state.results, info, nil
		}
		if ok != reported {
			reported = ok
			_, _ = fmt.Fprintf(deps.stderr, "verify: waiting: %d of %d step(s) ok, %s left\n",
				ok, len(steps), remaining.Round(time.Second))
		}
		cfg.clock.Sleep(ctx, minDuration(cfg.interval, remaining))
		if ctx.Err() != nil {
			info.Interrupted = true
			return state.results, info, nil
		}
	}
}

// lookRound looks one time at each step that is not final, and returns the
// new state. It does not change prev: the caller keeps prev when the round
// was cut by a canceled context.
func lookRound(ctx context.Context, deps stepDeps, steps []schema.PlanStep, prev waitState, now time.Time, settle time.Duration) waitState {
	next := prev.clone()
	for i, step := range steps {
		if prev.final[i] {
			continue
		}
		// A canceled context makes every read that is left fail with
		// the same error. The caller drops this round, so stop here and
		// do not print one "context canceled" line for each step.
		if ctx.Err() != nil {
			return next
		}
		check := checkStep(ctx, deps, step)
		if ctx.Err() != nil {
			return next
		}
		// A read error goes to stderr when it is new. A workload that
		// is not there gives the same error in every round, and one
		// line is enough.
		if check.result.Message != prev.results[i].Message {
			check.reportReadError(deps.stderr)
		}
		next.results[i], next.stable[i], next.final[i] = settleStep(ctx, deps, check, prev.stable[i], now, settle)
	}
	return next
}

// settleStep applies the "Ready must hold" rule to one look at one step. It
// returns the result to report for this round, what to remember, and
// whether the step is final (ok, not looked at again).
//
// When the pods have settled and the caller asked for the in-pod probe, the
// probe runs here: one time for a set of pods, on pods that are known to
// stay up.
func settleStep(ctx context.Context, deps stepDeps, check stepCheck, prev stability, now time.Time, need time.Duration) (schema.VerifyResult, stability, bool) {
	r := check.result
	if r.Status != schema.VerifyStatusOK {
		// Any look that is not ok starts the count again.
		return r, stability{}, false
	}
	if r.Readiness == nil {
		// An ok from the pod template: there are no pods that must
		// settle.
		return r, stability{}, true
	}

	fingerprint := podsFingerprint(check.pods)
	if prev.since.IsZero() || prev.fingerprint != fingerprint {
		// First time Ready, or the pods changed (one was replaced, or a
		// container started again).
		prev = stability{fingerprint: fingerprint, since: now}
	}
	held := now.Sub(prev.since)
	if held >= need {
		r.Readiness.Stable = true
		if deps.doProbe {
			prev.probed = probeSettled(ctx, deps, check.pods, prev.probed, &r)
		}
		return r, prev, r.Status == schema.VerifyStatusOK
	}

	// Ready now, and not for long enough. If the wait ends here, this is
	// what the report says. It is a mismatch, not ok: the verifier has
	// not seen the workload stay up.
	r.Status = schema.VerifyStatusMismatch
	r.Readiness.Message = fmt.Sprintf("the pods are Ready, but only for %s; with --wait they must stay Ready for %s with no restart",
		held.Round(time.Second), need)
	r.Message = r.Readiness.Message
	return r, prev, false
}

// probeSettled runs the in-pod probe for pods that have settled, or uses
// the answer that an earlier round got for the same pods. It writes the
// outcome into r and returns what to remember.
//
// An exec call that failed (a network error, a container with no shell) is
// not an answer about gVisor, so it is not remembered: the next round tries
// again. "No gVisor markers" is an answer. The step then stays a mismatch
// until the pods change or the wait ends.
func probeSettled(ctx context.Context, deps stepDeps, pods []corev1.Pod, known *probeOutcome, r *schema.VerifyResult) *probeOutcome {
	if known != nil {
		r.Status, r.Message, r.Probe = known.status, known.message, known.probe
		return known
	}
	applyProbeResult(ctx, deps.exec, pods, r)
	if r.Probe == nil || r.Probe.Error != "" {
		return nil
	}
	return &probeOutcome{status: r.Status, message: r.Message, probe: r.Probe}
}

func wholeSeconds(d time.Duration) int {
	return int(d.Round(time.Second) / time.Second)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
