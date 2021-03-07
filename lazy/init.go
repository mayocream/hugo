// Copyright 2019 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package lazy

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// New creates a new empty Init.
func New() *Init {
	return &Init{}
}

// Init holds a graph of lazily initialized dependencies.
type Init struct {
	mu sync.Mutex // 并发修改图的锁

	prev     *Init
	children []*Init

	init onceMore // 保证只执行一次的锁
	out  interface{} // 执行结果
	err  error // 执行错误
	f    func() (interface{}, error) // 回调函数
}

// Add adds a func as a new child dependency.
func (ini *Init) Add(initFn func() (interface{}, error)) *Init {
	if ini == nil {
		ini = New()
	}
	return ini.add(false, initFn)
}

// AddWithTimeout is same as Add, but with a timeout that aborts initialization.
func (ini *Init) AddWithTimeout(timeout time.Duration, f func(ctx context.Context) (interface{}, error)) *Init {
	return ini.Add(func() (interface{}, error) {
		return ini.withTimeout(timeout, f)
	})
}

// Branch creates a new dependency branch based on an existing and adds
// the given dependency as a child.
func (ini *Init) Branch(initFn func() (interface{}, error)) *Init {
	if ini == nil {
		ini = New()
	}
	return ini.add(true, initFn)
}

// BranchdWithTimeout is same as Branch, but with a timeout.
func (ini *Init) BranchWithTimeout(timeout time.Duration, f func(ctx context.Context) (interface{}, error)) *Init {
	return ini.Branch(func() (interface{}, error) {
		return ini.withTimeout(timeout, f)
	})
}

// Do initializes the entire dependency graph.
func (ini *Init) Do() (interface{}, error) {
	if ini == nil {
		panic("init is nil")
	}

	// 调用 onceMore 库保证只执行一次
	ini.init.Do(func() {
		// 获取父节点
		prev := ini.prev
		if prev != nil {
			// A branch. Initialize the ancestors.
			// 若父节点还没有完成初始化, 并且没有正在执行的回调函数, 执行
			if prev.shouldInitialize() {
				_, err := prev.Do()
				if err != nil {
					ini.err = err
					return
				}
			} else if prev.inProgress() {
				// Concurrent initialization. The following init func
				// may depend on earlier state, so wait.
				// 等待一定时间, 若没有执行完, panic
				prev.wait()
			}
		}

		// 执行回调函数
		if ini.f != nil {
			ini.out, ini.err = ini.f()
		}

		// 循环执行子节点的回调函数
		// 为什么不并发执行 ?
		for _, child := range ini.children {
			if child.shouldInitialize() {
				_, err := child.Do()
				if err != nil {
					ini.err = err
					return
				}
			}
		}
	})

	ini.wait()

	return ini.out, ini.err
}

// TODO(bep) investigate if we can use sync.Cond for this.
func (ini *Init) wait() {
	var counter time.Duration
	for !ini.init.Done() {
		counter += 10
		if counter > 600000000 {
			panic("BUG: timed out in lazy init")
		}
		time.Sleep(counter * time.Microsecond)
	}
}

func (ini *Init) inProgress() bool {
	return ini != nil && ini.init.InProgress()
}

// 若 没有注册了回调函数 | 已经完成 | 正在执行, 不进行初始化
func (ini *Init) shouldInitialize() bool {
	return !(ini == nil || ini.init.Done() || ini.init.InProgress())
}

// Reset resets the current and all its dependencies.
func (ini *Init) Reset() {
	mu := ini.init.ResetWithLock()
	defer mu.Unlock()
	for _, d := range ini.children {
		d.Reset()
	}
}

// 添加图的节点
func (ini *Init) add(branch bool, initFn func() (interface{}, error)) *Init {
	ini.mu.Lock()
	defer ini.mu.Unlock()

	// 如果是新建分支
	if branch {
		return &Init{
			f:    initFn,
			prev: ini, // 父节点
		}
	}

	// 如果是添加子节点
	// 如果已经被执行, panic
	ini.checkDone()
	// 添加子节点
	ini.children = append(ini.children, &Init{
		f: initFn,
	})

	// 释放锁
	return ini
}

func (ini *Init) checkDone() {
	if ini.init.Done() {
		panic("init cannot be added to after it has run")
	}
}

// callback 函数, 有超时时间
func (ini *Init) withTimeout(timeout time.Duration, f func(ctx context.Context) (interface{}, error)) (interface{}, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// 缓存通道, 防止阻塞
	c := make(chan verr, 1)

	go func() {
		v, err := f(ctx)
		select {
		case <-ctx.Done():
			return
		default:
			c <- verr{v: v, err: err}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, errors.New("timed out initializing value. You may have a circular loop in a shortcode, or your site may have resources that take longer to build than the `timeout` limit in your Hugo config file.")
	case ve := <-c:
		return ve.v, ve.err
	}
}

type verr struct {
	v   interface{}
	err error
}
