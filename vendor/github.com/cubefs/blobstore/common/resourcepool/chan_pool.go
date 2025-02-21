// Copyright 2022 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package resourcepool

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// cache in chan pool will not be released by runtime.GC().
// pool chan length dynamicly changes by EMA(Exponential Moving Average).
// redundant buffers will released by GC.

const maxMemorySize = 1 << 32 // 4G

var releaseInterval time.Duration = time.Second * 30

// SetReleaseInterval set release interval duration
func SetReleaseInterval(duration time.Duration) {
	if duration > time.Millisecond*100 {
		releaseInterval = duration
	}
}

type chPool struct {
	chBuffer    chan []byte
	newBuffer   func() []byte
	capacity    int
	concurrence int32

	closeCh   chan struct{}
	closeOnce sync.Once
}

// NewChanPool return Pool with capacity, no limit if capacity is negative
func NewChanPool(newFunc func() []byte, capacity int) Pool {
	chCap := capacity
	if chCap < 0 {
		buf := newFunc()
		chCap = maxMemorySize / len(buf)
	}

	pool := &chPool{
		chBuffer:  make(chan []byte, chCap),
		newBuffer: newFunc,
		capacity:  capacity,
		closeCh:   make(chan struct{}),
	}
	runtime.SetFinalizer(pool, func(p *chPool) {
		p.closeOnce.Do(func() {
			close(p.closeCh)
		})
	})

	go pool.loopRelease()
	return pool
}

// loopRelease release redundant buffers in chan.
// check EMA concurrence per one-tenth of release interval duration.
// release the redundant buffers per release interval duration.
//
// reserve 30% redundancy of capacity
//  - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -
//  |      length of buffer chan        |     concurrence     |
//  - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -
//  |  redundant  |            capacity            | reserved |
//  - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -
//  | to release  |          buffers keep in memory           |
//  - - - - - - - - - - - - - - - - - - - - - - - - - - - - - -
func (p *chPool) loopRelease() {
	timer := time.NewTicker(releaseInterval / 10)
	defer timer.Stop()

	var (
		turn     int
		capacity int32
	)
	for {
		select {
		case <-p.closeCh:
			for {
				select {
				case <-p.chBuffer:
				default:
					return
				}
			}
		case <-timer.C:
			nowConc := atomic.LoadInt32(&p.concurrence)
			capacity = ema(nowConc, capacity)
			if turn = (turn + 1) % 10; turn != 0 {
				continue
			}

			capa := capacity * 13 / 10
			redundant := len(p.chBuffer) + int(nowConc-capa)
			if redundant <= 0 {
				continue
			}
			for ii := 0; ii < redundant; ii++ {
				select {
				case <-p.chBuffer:
				default:
					break
				}
			}
		}
	}
}

func (p *chPool) Get() (interface{}, error) {
	atomic.AddInt32(&p.concurrence, 1)

	select {
	case buf := <-p.chBuffer:
		return buf, nil
	default:
		return p.newBuffer(), nil
	}
}

func (p *chPool) Put(x interface{}) {
	buf, ok := x.([]byte)
	if !ok {
		return
	}

	select {
	case p.chBuffer <- buf:
	default:
	}
	atomic.AddInt32(&p.concurrence, -1)
}

func (p *chPool) Cap() int {
	return p.capacity
}

func (p *chPool) Len() int {
	return int(atomic.LoadInt32(&p.concurrence))
}

func ema(val, lastVal int32) int32 {
	return (val*2 + lastVal*8) / 10
}
