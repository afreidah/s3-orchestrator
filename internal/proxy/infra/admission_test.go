// -------------------------------------------------------------------------------
// Backend Runtime - Admission Tests
//
// Author: Alex Freidah
//
// Background work shares the pool clients are admitted from. Clients only try
// for a slot, so these tests check the workers waiting on a full pool do not
// take every slot as it frees.
// -------------------------------------------------------------------------------

package infra

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/afreidah/s3-orchestrator/internal/util/batch"
)

// fullPool returns a runtime whose pool of size slots is held by clients.
func fullPool(size int) (*BackendRuntime, chan struct{}) {
	sem := make(chan struct{}, size)
	for range size {
		sem <- struct{}{}
	}
	return New(&Config{AdmissionSem: sem}), sem
}

// TestAdmission_FreedSlotReachesAClientWhileWorkersWait verifies only one worker
// lines up on a full pool: it takes the first slot to free, and the next one
// stays open for a client rather than going to the worker behind it.
func TestAdmission_FreedSlotReachesAClientWhileWorkersWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, sem := fullPool(2)
		finish := make(chan struct{})
		for range 3 {
			go batch.Admit(context.Background(), rt, func() { <-finish })
		}
		synctest.Wait()

		<-sem
		synctest.Wait()
		<-sem
		synctest.Wait()
		select {
		case sem <- struct{}{}:
		default:
			t.Error("a worker took the slot a client should have found free")
		}

		close(finish)
		<-sem
	})
}

// TestAdmission_WorkersLeaveABusyPoolToClients verifies workers take no more
// than the turn's slot while clients hold half the pool, so a slot a worker
// gives back is not taken straight back ahead of a client.
func TestAdmission_WorkersLeaveABusyPoolToClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, sem := fullPool(4)
		<-sem
		<-sem
		finish := make(chan struct{})
		var running atomic.Int32
		for range 3 {
			go batch.Admit(context.Background(), rt, func() {
				running.Add(1)
				<-finish
			})
		}
		time.Sleep(3 * admissionRetry)
		synctest.Wait()
		if got := running.Load(); got != 1 {
			t.Errorf("running = %d, want one worker on a pool clients hold half of", got)
		}
		select {
		case sem <- struct{}{}:
		default:
			t.Error("workers took the slot a client should have found free")
		}

		close(finish)
		for range 3 {
			<-sem
		}
	})
}

// TestAdmission_WorkerBehindTheTurnTakesAFreeSlot verifies a worker is not held
// up by a long item that holds the turn once the pool has room again.
func TestAdmission_WorkerBehindTheTurnTakesAFreeSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, sem := fullPool(4)
		finish := make(chan struct{})
		var running atomic.Int32
		for range 2 {
			go batch.Admit(context.Background(), rt, func() {
				running.Add(1)
				<-finish
			})
		}
		synctest.Wait()

		for range 4 {
			<-sem
		}
		time.Sleep(admissionRetry)
		synctest.Wait()
		if got := running.Load(); got != 2 {
			t.Errorf("running = %d, want both workers in the free slots", got)
		}
		close(finish)
	})
}

// TestAdmission_CancelledWaitGivesUpTheTurn verifies a worker cancelled while
// blocked on the pool passes the turn on, so the next worker can block.
func TestAdmission_CancelledWaitGivesUpTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, sem := fullPool(1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool)
		go func() {
			_, admitted := rt.AcquireAdmission(ctx)
			done <- admitted
		}()
		synctest.Wait()
		cancel()
		if <-done {
			t.Error("a cancelled worker was admitted to a full pool")
		}

		select {
		case rt.workerTurn <- struct{}{}:
		default:
			t.Error("the cancelled worker kept the turn")
		}
		<-sem
	})
}

// TestAdmission_CancelledBehindTheTurn verifies a worker waiting behind the
// turn gives up when ctx ends, without touching the pool.
func TestAdmission_CancelledBehindTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, sem := fullPool(1)
		rt.workerTurn <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, admitted := rt.AcquireAdmission(ctx); admitted {
			t.Error("a cancelled worker was admitted to a full pool")
		}
		<-sem
	})
}
