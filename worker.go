package dingo

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/mission-liao/dingo/common"
	"github.com/mission-liao/dingo/transport"
)

//
// errors
//

var (
	errWorkerNotFound = errors.New("Worker not found")
)

//
// worker
//
// a required record for a group of workers
//

type worker struct {
	tasks   chan *transport.Task
	rs      *common.Routines
	reports []chan *transport.Report
	fn      interface{}
}

//
// worker container
//

type _workers struct {
	lock           sync.Mutex
	workers        atomic.Value
	events         chan *common.Event
	eventConverter *common.Routines
	eventMux       *common.Mux
}

// allocating a new group of workers
//
// parameters:
// - id: identifier of this group of workers share the same function, matcher...
// - match: matcher of this group of workers
// - fn: the function that worker should called when receiving tasks (named by what recoginible by 'Matcher')
// - count: count of workers to be initiated
// - share: the count of workers sharing one report channel
// returns:
// - remain: count of workers remain not initiated
// - reports: array of channels of 'transport.Report'
// - err: any error
func (me *_workers) allocate(name string, fn interface{}, count, share int) (reports []<-chan *transport.Report, remain int, err error) {
	// make sure type of fn is relfect.Func
	k := reflect.TypeOf(fn).Kind()
	if k != reflect.Func {
		err = errors.New(fmt.Sprintf("Invalid function pointer passed: %v", k.String()))
		return
	}

	var (
		w   *worker
		eid int
	)
	defer func() {
		if err == nil {
			remain, reports, err = me.more(name, count, share)
		}

		if err != nil {
			if eid != 0 {
				_, err_ := me.eventMux.Unregister(eid)
				if err_ != nil {
					// TODO: log it
				}
			}

			if w != nil {
				err_ := w.rs.Close()
				if err_ != nil {
					// TODO: log it?
				}
				close(w.tasks)
			}
		}
	}()

	err = func() (err error) {
		me.lock.Lock()
		defer me.lock.Unlock()

		ws := me.workers.Load().(map[string]*worker)

		if _, ok := ws[name]; ok {
			err = errors.New(fmt.Sprintf("name %v exists", name))
			return
		}

		// initiate controlling channle
		w = &worker{
			tasks:   make(chan *transport.Task, 10), // TODO: configuration?
			rs:      common.NewRoutines(),
			fn:      fn,
			reports: make([]chan *transport.Report, 0, 10),
		}

		eid, err = me.eventMux.Register(w.rs.Events(), 0)
		if err != nil {
			return
		}

		// -- this line below should never throw any error --

		nws := make(map[string]*worker)
		for k := range ws {
			nws[k] = ws[k]
		}
		nws[name] = w
		me.workers.Store(nws)
		return
	}()

	return
}

// allocating more workers
//
// parameters:
// - name: identifier of this group of workers share the same function, matcher...
// - count: count of workers to be initiated
// - share: count of workers sharing one report channel
// returns:
// - remain: count of workers remain not initiated
// - err: any error
func (me *_workers) more(name string, count, share int) (remain int, reports []<-chan *transport.Report, err error) {
	remain = count
	if count <= 0 || share < 0 {
		err = errors.New(fmt.Sprintf("invalid count/share is provided %v", count, share))
		return
	}

	reports = make([]<-chan *transport.Report, 0, remain)

	// locking
	ws := me.workers.Load().(map[string]*worker)

	// checking existence of Id
	w, ok := ws[name]
	if !ok {
		err = errors.New(fmt.Sprintf("%d group of worker not found"))
		return
	}

	add := func() (r chan *transport.Report) {
		r = make(chan *transport.Report, 10)
		reports = append(reports, r)
		w.reports = append(w.reports, r)
		return
	}

	r := add()
	// initiating workers
	for ; remain > 0; remain-- {
		// re-initialize a report channel
		if share > 0 && remain != count && remain%share == 0 {
			r = add()
		}
		go me._worker_routine_(
			w.rs.New(),
			w.rs.Wait(),
			w.rs.Events(),
			w.tasks,
			r,
			w.fn,
		)
	}

	return
}

// dispatching a 'transport.Task'
//
// parameters:
// - t: the task
// returns:
// - err: any error
func (me *_workers) dispatch(t *transport.Task) (err error) {
	ws := me.workers.Load().(map[string]*worker)
	if v, ok := ws[t.Name()]; ok {
		v.tasks <- t
	} else {
		err = errWorkerNotFound
	}
	return
}

//
// common.Object interface
//

func (me *_workers) Events() ([]<-chan *common.Event, error) {
	return []<-chan *common.Event{
		me.events,
	}, nil
}

func (me *_workers) Close() (err error) {
	me.lock.Lock()
	defer me.lock.Unlock()

	// stop all workers routine
	ws := me.workers.Load().(map[string]*worker)
	for _, v := range ws {
		v.rs.Close()
		for _, r := range v.reports {
			close(r)
		}
	}
	me.workers.Store(make(map[string]*worker))

	return
}

// factory function
func newWorkers() (w *_workers, err error) {
	w = &_workers{
		eventConverter: common.NewRoutines(),
		events:         make(chan *common.Event, 10),
		eventMux:       common.NewMux(),
	}

	w.workers.Store(make(map[string]*worker))

	// a routine to mux multipls event channels from workers,
	// and output them through a single event channel.
	go func(quit <-chan int, wait *sync.WaitGroup, in <-chan *common.MuxOut, out chan *common.Event) {
		defer wait.Done()
		for {
			select {
			case _, _ = <-quit:
				goto clean
			case v, ok := <-in:
				if !ok {
					goto clean
				}
				out <- v.Value.(*common.Event)
			}
		}
	clean:
	}(w.eventConverter.New(), w.eventConverter.Wait(), w.eventMux.Out(), w.events)

	remain, err := w.eventMux.More(1)
	if err == nil && remain != 0 {
		err = errors.New(fmt.Sprintf("Unable to allocate mux routine:%v"))
	}

	return
}

//
// worker routine
//

func (me *_workers) _worker_routine_(quit <-chan int, wait *sync.WaitGroup, events chan<- *common.Event, tasks <-chan *transport.Task, reports chan<- *transport.Report, fn interface{}) {
	defer wait.Done()

	// TODO: concider a shared, common invoker instance?
	ivk := transport.NewDefaultInvoker()
	rep := func(task *transport.Task, status int, payload []interface{}, err error) {
		var (
			e    *transport.Error
			r    *transport.Report
			err_ error
		)
		if err != nil {
			e = transport.NewErr(0, err)
		}
		r, err_ = task.ComposeReport(status, payload, e)
		if err_ != nil {
			r, err_ = task.ComposeReport(transport.Status.Fail, nil, transport.NewErr(0, err_))
			if err_ != nil {
				events <- common.NewEventFromError(common.InstT.WORKER, err_)
				return
			}
		}

		reports <- r
	}
	call := func(t *transport.Task) {
		var (
			ret       []interface{}
			err, err_ error
			status    int
		)

		// compose a report -- sent
		rep(t, transport.Status.Sent, nil, nil)

		// compose a report -- progress
		rep(t, transport.Status.Progress, nil, nil)

		// call the actuall function, where is the magic
		ret, err = ivk.Invoke(fn, t.Args())

		// compose a report -- done / fail
		if err != nil {
			status = transport.Status.Fail
			events <- common.NewEventFromError(common.InstT.WORKER, err_)
		} else {
			status = transport.Status.Done
		}
		rep(t, status, ret, err)
	}

	for {
		select {
		case t, ok := <-tasks:
			if !ok {
				goto clean
			}

			call(t)
		case _, _ = <-quit:
			// nothing to clean
			goto clean
		}
	}
clean:
	finished := false
	for {
		select {
		case t, ok := <-tasks:
			if !ok {
				finished = true
				break
			}
			call(t)
		default:
			finished = true
		}
		if finished {
			break
		}
	}
}
