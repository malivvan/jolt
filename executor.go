package jolt

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Executor compiles a program once and runs it in a pool of VMs. It is
// safe for concurrent use. Call and Eval take an idle VM or create one,
// up to Config.PoolSize.
type Executor struct {
	cfg  Config
	prog *program
	pool *vmPool

	closed atomic.Bool
}

// New compiles cfg's program and returns an executor. The program and its
// imports are compiled and linked once; VMs are created lazily.
func New(cfg Config) (*Executor, error) {
	defaults, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	prog, err := newProgram(defaults)
	if err != nil {
		return nil, err
	}
	return &Executor{
		cfg:  defaults,
		prog: prog,
		pool: newVMPool(defaults, prog),
	}, nil
}

// NewVM compiles cfg's program and creates one initialized runtime. The
// caller owns it and must Close it. Use New when several goroutines call the
// same program.
func NewVM(cfg Config) (*VM, error) {
	defaults, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	prog, err := newProgram(defaults)
	if err != nil {
		return nil, err
	}
	return prog.newVM()
}

// Config returns the executor's configuration.
func (e *Executor) Config() Config { return e.cfg }

// Call invokes the function at path in one runtime and converts the result to
// a Go value. A returned promise is awaited unless
// Config.DisableAwaitPromises is set.
func (e *Executor) Call(ctx context.Context, path string, args ...any) (any, error) {
	var out any
	err := e.Run(ctx, func(rt *VM) error {
		v, err := rt.Call(ctx, path, args...)
		if err != nil {
			return err
		}
		out, err = v.ToGo()
		return err
	})
	return out, err
}

// CallInto calls path and decodes the result into out, a non-nil pointer.
func (e *Executor) CallInto(ctx context.Context, path string, out any, args ...any) error {
	return e.Run(ctx, func(rt *VM) error {
		return rt.CallInto(ctx, path, out, args...)
	})
}

// Eval runs source as a classic script in one runtime and converts the
// completion value to a Go value.
func (e *Executor) Eval(ctx context.Context, source string) (any, error) {
	var out any
	err := e.Run(ctx, func(rt *VM) error {
		v, err := rt.Eval(ctx, source)
		if err != nil {
			return err
		}
		out, err = v.ToGo()
		return err
	})
	return out, err
}

// EvalInto evaluates source and decodes its completion value into out.
func (e *Executor) EvalInto(ctx context.Context, source string, out any) error {
	return e.Run(ctx, func(rt *VM) error {
		return rt.EvalInto(ctx, source, out)
	})
}

// Get resolves a dotted path and converts the value to Go.
func (e *Executor) Get(ctx context.Context, path string) (any, error) {
	var out any
	err := e.Run(ctx, func(rt *VM) error {
		v, err := rt.Get(path)
		if err != nil {
			return err
		}
		out, err = v.ToGo()
		return err
	})
	return out, err
}

// GetInto resolves a dotted path and decodes the value into out.
func (e *Executor) GetInto(ctx context.Context, path string, out any) error {
	return e.Run(ctx, func(rt *VM) error {
		v, err := rt.Get(path)
		if err != nil {
			return err
		}
		return v.Into(out)
	})
}

// Run leases one VM for the duration of fn. The VM and its Value
// handles are valid only inside fn. A panic in fn propagates after the VM
// is returned or closed.
func (e *Executor) Run(ctx context.Context, fn func(*VM) error) error {
	if e.closed.Load() {
		return ErrClosed
	}
	if fn == nil {
		return errors.New("jolt: nil Run function")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rt, err := e.pool.acquire(ctx)
	if err != nil {
		return err
	}
	reuse := func() bool {
		return !e.cfg.NoReuse && !rt.broken.Load() && !rt.closed.Load()
	}
	defer func() {
		e.pool.release(rt, reuse())
	}()
	return fn(rt)
}

// Close closes the executor and the idle VMs. Calls already running keep
// their VM; it is closed when they release it. Close is idempotent.
func (e *Executor) Close() error {
	if e == nil {
		return nil
	}
	if !e.closed.CompareAndSwap(false, true) {
		return nil
	}
	e.pool.close()
	return nil
}

// vmPool bounds concurrency and keeps idle VMs.
type vmPool struct {
	prog    *program
	sem     chan struct{}
	maxIdle int
	mu      sync.Mutex
	idle    []*VM
	closed  bool
}

func newVMPool(cfg Config, prog *program) *vmPool {
	size := cfg.PoolSize
	if size < 1 {
		size = 1
	}
	return &vmPool{
		prog:    prog,
		sem:     make(chan struct{}, size),
		maxIdle: cfg.MaxIdle,
	}
}

func (p *vmPool) acquire(ctx context.Context) (*VM, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.sem
		return nil, ErrClosed
	}
	if n := len(p.idle); n > 0 {
		rt := p.idle[n-1]
		p.idle[n-1] = nil
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return rt, nil
	}
	p.mu.Unlock()

	rt, err := p.prog.newVM()
	if err != nil {
		<-p.sem
		return nil, err
	}
	return rt, nil
}

func (p *vmPool) release(rt *VM, reuse bool) {
	p.mu.Lock()
	if reuse && !p.closed && p.maxIdle > 0 && len(p.idle) < p.maxIdle {
		p.idle = append(p.idle, rt)
		p.mu.Unlock()
		<-p.sem
		return
	}
	p.mu.Unlock()
	_ = rt.Close()
	<-p.sem
}

func (p *vmPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, rt := range idle {
		_ = rt.Close()
	}
}
