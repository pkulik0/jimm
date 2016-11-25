// Copyright 2016 Canonical Ltd.

package monitor

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/juju/juju/state/multiwatcher"
	"github.com/juju/utils/parallel"
	"golang.org/x/net/context"
	"gopkg.in/errgo.v1"
	"gopkg.in/tomb.v2"

	"github.com/CanonicalLtd/jem/internal/jem"
	"github.com/CanonicalLtd/jem/internal/mongodoc"
	"github.com/CanonicalLtd/jem/internal/servermon"
	"github.com/CanonicalLtd/jem/params"
)

var errControllerRemoved = errgo.New("controller has been removed")

// maxConcurrentUpdates holds the maximum number of
// concurrent database operations that a given
// controller monitor may make.
const maxConcurrentUpdates = 10

// controllerMonitor is responsible for monitoring a single
// controller.
type controllerMonitor struct {
	// tomb is killed when the controller being monitored
	// has been removed.
	tomb tomb.Tomb

	// context holds the monitor's context.
	context context.Context

	// leaseExpiry holds the time that the currently held lease
	// will expire. It is maintained by the leaseUpdater goroutine.
	leaseExpiry time.Time

	// jem holds the current JEM database connection.
	jem jemInterface

	// ctlPath holds the path of the controller we're monitoring.
	ctlPath params.EntityPath

	// ownerId holds this agent's name, the owner of the lease.
	ownerId string
}

// controllerMonitorParams holds parameters for creating
// a new controller monitor.
type controllerMonitorParams struct {
	jem         jemInterface
	ctlPath     params.EntityPath
	ownerId     string
	leaseExpiry time.Time
}

// newControllerMonitor starts a new monitor to monitor one controller.
func newControllerMonitor(ctx context.Context, p controllerMonitorParams) *controllerMonitor {
	m := &controllerMonitor{
		jem:         p.jem,
		ctlPath:     p.ctlPath,
		ownerId:     p.ownerId,
		leaseExpiry: p.leaseExpiry,
	}
	m.context = newTombContext(ctx, &m.tomb)
	m.tomb.Go(func() error {
		m.tomb.Go(m.leaseUpdater)
		m.tomb.Go(m.watcher)
		return nil
	})
	servermon.ControllersRunning.Inc()
	return m
}

// Kill implements worker.Worker.Kill by killing the controller monitor.
func (m *controllerMonitor) Kill() {
	m.tomb.Kill(nil)
}

// Wait implements worker.Worker.Wait by waiting for
// the controller monitor to terminate.
func (m *controllerMonitor) Wait() error {
	return m.tomb.Wait()
}

// Dead returns a channel which is closed when the controller
// monitor has terminated.
func (m *controllerMonitor) Dead() <-chan struct{} {
	return m.tomb.Dead()
}

// leaseUpdater is responsible for updating the controller's lease
// as long as we still have the lease, the controller still exists,
// and the monitor is still alive.
func (m *controllerMonitor) leaseUpdater() error {
	for {
		// Renew after ¾ of the lease time has passed.
		renewTime := m.leaseExpiry.Add(-leaseExpiryDuration / 4)
		select {
		case <-Clock.After(renewTime.Sub(Clock.Now())):
		case <-m.tomb.Dying():
			// Try to drop the lease because the monitor might
			// not be starting again on this JEM instance.
			if err := m.renewLease(false); err != nil {
				return errgo.NoteMask(err, "cannot drop lease", isMonitoringStoppedError)
			}
			return tomb.ErrDying
		}
		// It's time to renew the lease.
		if err := m.renewLease(true); err != nil {
			msg := fmt.Sprintf("cannot renew lease on %v", m.ctlPath)
			return errgo.NoteMask(err, msg, isMonitoringStoppedError)
		}
	}
}

// renewLease renews the lease (or drops it if renew is false)
// and updates the m.leaseExpiry to be the new lease expiry time.
//
// If the lease cannot be renewed because someone else
// has acquired it, it returns an error with a jem.Err or the controller has been removed,
// it returns an error with a cause that satisfies isMonitoringStoppedError.
func (m *controllerMonitor) renewLease(renew bool) error {
	var ownerId string
	if renew {
		ownerId = m.ownerId
	}
	t, err := acquireLease(m.jem, m.ctlPath, m.leaseExpiry, m.ownerId, ownerId)
	if err == nil {
		logger.Debugf("controller %v acquired lease successfully (new time %v)", m.ctlPath, t)
		m.leaseExpiry = t
		return nil
	}
	logger.Infof("controller %v acquire lease failed: %v", m.ctlPath, err)
	return errgo.Mask(err, isMonitoringStoppedError)
}

// acquireLease is like jem.JEM.AcquireMonitorLease except that
// it returns errControllerRemoved if the controller has been
// removed or jem.ErrLeaseUnavailable if the lease is unavailable,
// and it always acquires a lease leaseExpiryDuration from now.
func acquireLease(j jemInterface, ctlPath params.EntityPath, oldExpiry time.Time, oldOwner, newOwner string) (time.Time, error) {
	t, err := j.AcquireMonitorLease(ctlPath, oldExpiry, oldOwner, Clock.Now().Add(leaseExpiryDuration), newOwner)
	if err == nil {
		return t, nil
	}
	if errgo.Cause(err) == params.ErrNotFound {
		err = errControllerRemoved
	}
	return time.Time{}, errgo.Mask(err, isMonitoringStoppedError)
}

// watcher runs the controller monitor watcher itself.
// It returns an error satisfying isMonitoringStoppedError if
// the controller is removed.
func (m *controllerMonitor) watcher() error {
	for {
		logger.Debugf("monitor dialing controller %v", m.ctlPath)
		dialStartTime := Clock.Now()
		conn, err := m.dialAPI()
		switch errgo.Cause(err) {
		case nil:
			if err := m.jem.SetControllerAvailable(m.ctlPath); err != nil {
				return errgo.Notef(err, "cannot set controller availability")
			}

			if err := m.jem.ControllerUpdateCredentials(m.context, m.ctlPath); err != nil {
				return errgo.Notef(err, "cannot update credentials")
			}

			err = m.watch(conn)
			if errgo.Cause(err) == tomb.ErrDying {
				conn.Close()
				return tomb.ErrDying
			}
			logger.Infof("watch controller %v died: %v", m.ctlPath, err)
			// The problem is almost certainly with the controller
			// API connection, so evict the connection from the API
			// cache so we'll definitely re-dial the controller rather
			// than reusing the connection from the cache.
			conn.Evict()
		case tomb.ErrDying:
			// The controller has been removed or we've been explicitly stopped.
			return tomb.ErrDying
		case jem.ErrAPIConnection:
			if err := m.jem.SetControllerUnavailableAt(m.ctlPath, dialStartTime); err != nil {
				return errgo.Notef(err, "cannot set controller availability")
			}
			// We've failed to connect to the API. Log the error and
			// try again.
			// TODO update the controller doc with the error?
			logger.Errorf("cannot connect to controller %v: %v", m.ctlPath, err)
		default:
			// Some other error has happened. Don't mask the monitor-stopped
			// error that occurs if the controller is removed, because
			// we want the controller monitor to die quietly in that case.
			return errgo.NoteMask(err, fmt.Sprintf("cannot dial API for controller %v", m.ctlPath), isMonitoringStoppedError)
		}
		// Sleep for a while so we don't batter the network.
		// TODO exponentially backoff up to some limit.
		select {
		case <-m.tomb.Dying():
			// The controllerMonitor is dying.
			return tomb.ErrDying
		case <-Clock.After(apiConnectRetryDuration):
		}
	}
}

// dialAPI makes an API connection while also monitoring for shutdown.
// If the tomb starts dying while dialing, it returns tomb.ErrDying. If
// we can't make an API connection because the controller has been
// removed, it returns an error with an errControllerRemoved cause. If it
// can't make a connection because the dial itself failed, it returns an
// error with a jem.ErrAPIConnection cause.
func (m *controllerMonitor) dialAPI() (jujuAPI, error) {
	type apiConnReply struct {
		conn jujuAPI
		err  error
	}
	reply := make(chan apiConnReply)
	// Make an independent copy of the JEM instance
	// because this goroutine might live on beyond
	// the allMonitor's lifetime.
	j := m.jem.Clone()
	go func() {
		// Open the API to the controller's admin model.
		conn, err := j.OpenAPI(m.context, m.ctlPath)

		// Close before sending the reply rather than deferring
		// so that if our reply causes everything to be stopped,
		// we know that the JEM is closed before that.
		j.Close()
		logger.Infof("openAPI returned error %v", err)
		select {
		case reply <- apiConnReply{
			conn: conn,
			err:  err,
		}:
		case <-m.tomb.Dying():
			if conn != nil {
				conn.Close()
			}
		}
	}()
	select {
	case r := <-reply:
		if errgo.Cause(r.err) == params.ErrNotFound {
			r.err = errControllerRemoved
		}
		return r.conn, errgo.Mask(r.err, isMonitoringStoppedError, errgo.Is(jem.ErrAPIConnection))
	case <-m.tomb.Dying():
		return nil, tomb.ErrDying
	}
}

// watch reads events from the API megawatcher and
// updates runtime stats in the controller document in response
// to those.
func (m *controllerMonitor) watch(conn jujuAPI) error {
	apiw, err := conn.WatchAllModels()
	if err != nil {
		return errgo.Notef(err, "cannot watch all models")
	}
	defer apiw.Stop()

	w := newWatcherState(m.jem, m.ctlPath)
	type reply struct {
		deltas []multiwatcher.Delta
		err    error
	}
	replyc := make(chan reply, 1)
	for {
		go func() {
			// Ideally rpc.Client would have a Go method
			// similar to net/rpc's Go method, so we could
			// avoid making a goroutine each time, but currently
			// it does not.
			d, err := apiw.Next()
			replyc <- reply{d, err}
		}()
		var r reply
		select {
		case r = <-replyc:
		case <-m.tomb.Dying():
			return tomb.ErrDying
		}
		if r.err != nil {
			return errgo.Notef(r.err, "watcher error waiting for next event")
		}
		w.changed = false
		w.runner = parallel.NewRun(maxConcurrentUpdates)
		for _, d := range r.deltas {
			if err := w.addDelta(d); err != nil {
				return errgo.Mask(err)
			}
		}
		logger.Infof("controller %v: all deltas processed", w.ctlPath)
		if w.changed {
			w.runner.Do(func() error {
				if err := m.jem.SetControllerStats(m.ctlPath, &w.stats); err != nil {
					return errgo.Notef(err, "cannot set controller stats")
				}
				return nil
			})
		}
		// TODO perform all these updates concurrently?
		for uuid, info := range w.models {
			uuid, info := uuid, info
			// TODO(rogpeppe) When both unit count and life change, we could
			// combine them into a single database update.
			if info.changed&lifeChange != 0 {
				w.runner.Do(func() error {
					if err := w.jem.SetModelLife(w.ctlPath, uuid, string(info.life)); err != nil {
						return errgo.Notef(err, "cannot update model life")
					}
					return nil
				})
			}
			if info.changed&countsChange != 0 {
				w.runner.Do(func() error {
					// Note: if we get a "not found" error, ignore it because it is expected that
					// some models (e.g. the controller model) will not have a record in the
					// database.
					if err := m.jem.UpdateModelCounts(uuid, info.counts, time.Now()); err != nil && errgo.Cause(err) != params.ErrNotFound {
						return errgo.Notef(err, "cannot update model counts")
					}
					return nil
				})
			}
			info.changed = 0
		}
		// Wait for all the database updates to complete.
		if err := w.runner.Wait(); err != nil {
			return errgo.Mask(err)
		}
		w.runner = nil
	}
}

// watcherState holds the state that's maintained when watching
// a controller.
type watcherState struct {
	jem jemInterface

	// runner is used to start concurrent operations
	// while updating deltas.
	runner *parallel.Run

	// entities holds a map from entity tag to whether it exists.
	entities map[multiwatcher.EntityId]bool

	// ctlPath holds the path to the controller.
	ctlPath params.EntityPath

	// changed holds whether the stats have been updated
	// since the last time it was set to false.
	changed bool

	// stats holds the current known statistics about the controller.
	stats mongodoc.ControllerStats

	// models holds information about the models hosted by the controller.
	models map[string]*modelInfo
}

type modelChange int

const (
	lifeChange modelChange = 1 << iota
	countsChange
)

// modelInfo holds information on a model.
type modelInfo struct {
	uuid string

	// life holds the lifecycle status of the model.
	life multiwatcher.Life

	// counts holds current counts for entities in the model.
	counts map[params.EntityCount]int

	// changed holds information about what's changed
	// in the model since the last set of deltas.
	changed modelChange
}

func (info *modelInfo) adjustCount(kind params.EntityCount, n int) {
	if n != 0 {
		info.counts[kind] += n
		info.changed |= countsChange
	}
}

func (info *modelInfo) setLife(life multiwatcher.Life) {
	if life != info.life {
		info.life = life
		info.changed |= lifeChange
	}
}

func newWatcherState(j jemInterface, ctlPath params.EntityPath) *watcherState {
	return &watcherState{
		jem:     j,
		ctlPath: ctlPath,

		// models maps from model UUID to information about that model.
		models: make(map[string]*modelInfo),

		// entities holds an entry for each entity in the controller
		// so that we can tell the difference between change and
		// creation.
		entities: make(map[multiwatcher.EntityId]bool),
	}
}

func (w *watcherState) addDelta(d multiwatcher.Delta) error {
	if logger.IsDebugEnabled() {
		id := d.Entity.EntityId()
		if d.Removed {
			logger.Infof("controller %v got delta %v - removed %s-%s", w.ctlPath, id.ModelUUID, id.Kind, id.Id)
		} else {
			data, err := json.Marshal(d.Entity)
			if err != nil {
				data = []byte("cannot marshal")
			}
			logger.Infof("controller %v got delta %v - changed %s-%s: %s", w.ctlPath, id.ModelUUID, id.Kind, id.Id, data)
		}
	}
	ctlpathstr := string(w.ctlPath.Name) + ":" + string(w.ctlPath.User)
	switch e := d.Entity.(type) {
	case *multiwatcher.ModelInfo:
		// Ensure there's always a model entry.
		w.adjustCount(&w.stats.ModelCount, d)
		life := multiwatcher.Life("dead")
		if !d.Removed {
			life = e.Life
		}
		w.modelInfo(e.ModelUUID).setLife(life)
		servermon.ModelsRunning.WithLabelValues(ctlpathstr).Set(float64(w.stats.ModelCount))
	case *multiwatcher.UnitInfo:
		delta := w.adjustCount(&w.stats.UnitCount, d)
		w.modelInfo(e.ModelUUID).adjustCount(params.UnitCount, delta)
		servermon.UnitsRunning.WithLabelValues(ctlpathstr).Set(float64(w.stats.UnitCount))
	case *multiwatcher.ApplicationInfo:
		delta := w.adjustCount(&w.stats.ServiceCount, d)
		w.modelInfo(e.ModelUUID).adjustCount(params.ApplicationCount, delta)
		servermon.ApplicationsRunning.WithLabelValues(ctlpathstr).Set(float64(w.stats.ServiceCount))
	case *multiwatcher.MachineInfo:
		// TODO for top level machines, increment instance count?
		delta := w.adjustCount(&w.stats.MachineCount, d)
		w.modelInfo(e.ModelUUID).adjustCount(params.MachineCount, delta)
		servermon.MachinesRunning.WithLabelValues(ctlpathstr).Set(float64(w.stats.MachineCount))
		w.runner.Do(func() error {
			return w.jem.UpdateMachineInfo(e)
		})
	}
	return nil
}

// modelInfo returns the info value for the model with the given UUID,
// creating it if needed. The creation is needed because we
// may receive information on a unit before we receive information
// on its model but we still want the model to be updated appropriately.
func (w watcherState) modelInfo(uuid string) *modelInfo {
	info := w.models[uuid]
	if info == nil {
		info = &modelInfo{
			uuid: uuid,
			// Always create with everything changed so that we will
			// update the counts even if no entities are created.
			changed: ^0,
			counts: map[params.EntityCount]int{
				params.UnitCount:        0,
				params.MachineCount:     0,
				params.ApplicationCount: 0,
			},
		}
		w.models[uuid] = info
	}
	return info
}

// adjustCount increments or decrements the value pointed
// to by n depending on whether delta.Removed is true.
// It sets w.changed to true to indicate that something has
// changed and keeps track of whether the entity id exists.
//
// It returns the actual delta the count has been adjusted by.
func (w *watcherState) adjustCount(n *int, delta multiwatcher.Delta) int {
	id := delta.Entity.EntityId()
	diff := 0
	if delta.Removed {
		// Technically there's no need for the test here as we shouldn't
		// get two Removes in a row, but let's be defensive.
		if w.entities[id] {
			delete(w.entities, id)
			diff = -1
		}
	} else if !w.entities[id] {
		w.entities[id] = true
		diff = 1
	}
	if diff != 0 {
		*n += diff
		w.changed = true
	}
	return diff
}

func isMonitoringStoppedError(err error) bool {
	cause := errgo.Cause(err)
	return cause == errControllerRemoved || cause == jem.ErrLeaseUnavailable
}
