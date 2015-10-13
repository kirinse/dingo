package broker

import (
	"encoding/json"
	"sync"

	"github.com/mission-liao/dingo/common"
	"github.com/mission-liao/dingo/task"
)

type _local struct {
	// broker routine
	brk    *common.RtControl
	to     chan []byte
	noJSON chan task.Task
	tasks  chan task.Task
	errs   chan error
	bypass bool

	// monitor routine
	monitor    *common.RtControl
	muxReceipt *common.Mux
	uhLock     sync.Mutex
	unhandled  map[string]task.Task
	fLock      sync.RWMutex
	failures   []*Receipt
	rid        int
}

// factory
func newLocal(bypass bool) (v *_local) {
	v = &_local{
		brk:    common.NewRtCtrl(),
		to:     make(chan []byte, 10),
		noJSON: make(chan task.Task, 10),
		tasks:  make(chan task.Task, 10),
		errs:   make(chan error, 10),
		bypass: bypass,

		monitor:    common.NewRtCtrl(),
		muxReceipt: &common.Mux{},
		unhandled:  make(map[string]task.Task),
		failures:   make([]*Receipt, 0, 10),
		rid:        0,
	}

	v.init()
	return
}

func (me *_local) init() {
	me.muxReceipt.Init()

	// output function of broker routine
	out := func(t task.Task) {
		me.tasks <- t

		func() {
			me.uhLock.Lock()
			defer me.uhLock.Unlock()

			me.unhandled[t.GetId()] = t
		}()

		return
	}

	// broker routine
	go func(quit <-chan int, done chan<- int) {
		for {
			select {
			case _, _ = <-quit:
				done <- 1
			case v, ok := <-me.noJSON:
				if !ok {
					break
				}
				out(v)
			case v, ok := <-me.to:
				if !ok {
					// TODO: ??
					break
				}

				t_, err_ := task.UnmarshalTask(v)
				if err_ != nil {
					me.errs <- err_
					break
				}
				out(t_)
			}
		}
	}(me.brk.Quit, me.brk.Done)

	// start a new monitor routine
	go func(quit <-chan int, done chan<- int) {
		for {
			select {
			case _, _ = <-quit:
				done <- 1
				return
			case v, ok := <-me.muxReceipt.Out():
				if !ok {
					// mux is closed
					done <- 1
					return
				}

				out, valid := v.Value.(Receipt)
				if !valid {
					// TODO: log it
					break
				}
				if out.Status != Status.OK {
					func() {
						me.fLock.Lock()
						defer me.fLock.Unlock()

						// TODO: providing interface to access
						// these errors

						// catch this error recepit
						me.failures = append(me.failures, &out)
					}()
				}

				func() {
					me.uhLock.Lock()
					defer me.uhLock.Unlock()

					// stop monitoring
					delete(me.unhandled, out.Id)
				}()
			}
		}
	}(me.monitor.Quit, me.monitor.Done)
}

//
// constructor / destructor
//

func (me *_local) Close() {
	me.brk.Close()
	me.monitor.Close()
	close(me.to)
	close(me.noJSON)
	close(me.tasks)
	close(me.errs)
	me.muxReceipt.Close()
}

//
// Producer
//

func (me *_local) Send(t task.Task) (err error) {
	if me.bypass {
		me.noJSON <- t
		return
	}

	// marshal
	body, err := json.Marshal(t)
	if err != nil {
		return
	}

	me.to <- body
	return
}

//
// Consumer
//

func (me *_local) Consume(rcpt <-chan Receipt) (tasks <-chan task.Task, errs <-chan error, err error) {
	me.rid, err = me.muxReceipt.Register(rcpt)
	tasks, errs = me.tasks, me.errs
	return
}

func (me *_local) Stop() (err error) {
	_, err = me.muxReceipt.Unregister(me.rid)
	me.rid = 0
	return
}
