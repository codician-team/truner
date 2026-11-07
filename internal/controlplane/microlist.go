package controlplane

import (
	"context"
	"errors"
	"time"

	mvmtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
)

// ListMicrovms is rate limited per account, and installations in one account
// share the limit. It returns every stack's MicroVMs, terminated ones too, in
// pages of 10 unless asked for more: 104 terminated MicroVMs took 11 calls.
// So launches count MicroVMs from the job records, and the janitor lists them
// only while a record holds one, plus now and then to find MicroVMs no record
// knows about.
const (
	microListPage   = 50 // the most ListMicrovms returns per page
	microSweepEvery = 15 * time.Minute
	microMaxLife    = 8 * time.Hour // Lambda ends a MicroVM by then
	microBackoffPK  = "microvm-list-backoff"
)

// holdsMicroVM reports whether the MicroVM launched for this record may still
// be up.
func (j *Job) holdsMicroVM(now time.Time) bool {
	if !isMicroVM(j.InstanceID) || j.MachineDone != 0 || now.Sub(time.Unix(j.LaunchedAt, 0)) >= microMaxLife {
		return false
	}
	// A cancelled job's MicroVM is ended, unless another job runs on it.
	return j.PickedBy != 0 || j.Status != StatusCancelled && j.Status != StatusFailed
}

// microInUse reports whether this stack has a MicroVM image: without one it
// has no MicroVMs, and regions without MicroVMs see no failing calls.
func (cp *ControlPlane) microInUse(ctx context.Context) (bool, error) {
	if !cp.MicroEnabled() {
		return false, nil
	}
	items, err := cp.Store.ItemsInColl(ctx, collMicro)
	return len(items) > 0, err
}

// heldMicrovms returns the MicroVMs that job records, and machines kept apart
// from their job, hold.
func (cp *ControlPlane) heldMicrovms(ctx context.Context) (map[string]bool, error) {
	jobs, err := cp.recentJobs(ctx)
	if err != nil {
		return nil, err
	}
	kept, err := cp.Store.JobsInColl(ctx, collMachines)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	now := time.Now()
	for _, list := range [][]Job{jobs, kept} {
		for i := range list {
			if list[i].holdsMicroVM(now) {
				held[list[i].InstanceID] = true
			}
		}
	}
	return held, nil
}

// countMicrovms counts this stack's MicroVMs for the instance limit from the
// records, so a throttled list cannot leave them out. MicroVMs no record knows
// about are missing until the janitor ends them. When the records cannot be
// read, the last count stands in.
func (cp *ControlPlane) countMicrovms(ctx context.Context) int {
	n := 0
	inUse, err := cp.microInUse(ctx)
	if err == nil && inUse {
		var held map[string]bool
		held, err = cp.heldMicrovms(ctx)
		n = len(held)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if err != nil {
		cp.Log.Warn("could not count MicroVMs, using the last count", "err", err, "last", cp.microLast)
		return cp.microLast
	}
	cp.microLast = n
	return n
}

// janitorMicrovms returns what the janitor needs of ListMicrovms: nil when
// liveness is unknown (the list failed or is backing off), empty when no
// MicroVM is up.
func (cp *ControlPlane) janitorMicrovms(ctx context.Context) (map[string]mvmtypes.MicrovmItem, error) {
	none := map[string]mvmtypes.MicrovmItem{}
	inUse, err := cp.microInUse(ctx)
	if err != nil {
		return nil, err
	}
	if !inUse {
		return none, nil
	}
	backoff, err := cp.Store.GetItem(ctx, microBackoffPK)
	if err != nil {
		return nil, err
	}
	if backoff != nil && time.Now().Unix() < backoff.Int("until") {
		cp.Log.Info("not listing MicroVMs yet: the list was throttled", "until", time.Unix(backoff.Int("until"), 0).UTC())
		return nil, nil
	}
	held, err := cp.heldMicrovms(ctx)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 && !cp.everyN(ctx, "microvm-sweep", microSweepEvery) {
		return none, nil
	}
	live, err := cp.LiveMicrovms(ctx)
	if err != nil {
		cp.backOffMicroList(ctx, err)
		return nil, err
	}
	if backoff != nil {
		cp.Store.DeleteItem(ctx, microBackoffPK)
	}
	return live, nil
}

// backOffMicroList pauses the janitor's list after throttling, for all of this
// stack's Lambda instances.
func (cp *ControlPlane) backOffMicroList(ctx context.Context, err error) {
	var retryAfter time.Duration
	var throttled *mvmtypes.ThrottlingException
	var tooMany *mvmtypes.TooManyRequestsException
	switch {
	case errors.As(err, &throttled):
		if throttled.RetryAfterSeconds != nil {
			retryAfter = time.Duration(*throttled.RetryAfterSeconds) * time.Second
		}
	case !errors.As(err, &tooMany):
		return
	}
	n, aerr := cp.Store.AddNumber(ctx, microBackoffPK, "n", 1, time.Hour)
	if aerr == nil {
		aerr = cp.Store.UpdateItem(ctx, microBackoffPK, Item{"until": time.Now().Add(microListWait(int(n), retryAfter)).Unix()})
	}
	if aerr != nil {
		cp.Log.Warn("could not record the MicroVM list's back-off", "err", aerr)
	}
}

// microListWait is how long the janitor waits after the nth throttled list in
// a row. The janitor runs every 2 minutes.
func microListWait(n int, retryAfter time.Duration) time.Duration {
	return max(2*time.Minute<<min(n, 3), retryAfter)
}
