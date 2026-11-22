package js

import (
	"io"
	"sync"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/eventloop"
)

// Feature is one composable piece of a runtime: a file system, a network, a
// set of globals, or anything else the host wants to install. Features are
// installed in every runtime after the event loop exists and before the
// program loads. Install must be safe to call concurrently for different
// runtimes.
type Feature interface {
	// Install adds the feature to env and returns a cleanup function, which
	// may be nil. Install runs on the runtime's goroutine, outside any
	// JavaScript call. The cleanup runs when the runtime closes.
	Install(env *Env) (cleanup func() error, err error)
}

// FeatureFunc adapts a function to Feature.
type FeatureFunc func(*Env) (cleanup func() error, err error)

// Install calls f.
func (f FeatureFunc) Install(env *Env) (func() error, error) { return f(env) }

// ResolverProvider is implemented by a feature that resolves import
// specifiers, such as a file system. The feature's resolver is asked before
// the one it wraps, so features earlier in Config's install order win.
type ResolverProvider interface {
	Resolver(next moejs.Resolver) moejs.Resolver
}

// ImporterProvider is implemented by a feature that wraps the importer used
// for import() and import.meta, such as node:fs. Earlier features are the
// outer wrappers, so they win for the specifiers they know.
type ImporterProvider interface {
	Importer(next *moejs.Importer) *moejs.Importer
}

// FS is a file system feature. HostFS and NoFS implement it; any Feature
// works as a file system.
type FS interface {
	Feature
}

// Network is a network feature. HostNetwork and NoNetwork implement it; any
// Feature works as a network.
type Network interface {
	Feature
}

// NoFS installs no file system. Importing node:fs fails.
type NoFS struct{}

// Install does nothing.
func (NoFS) Install(*Env) (func() error, error) { return nil, nil }

// NoNetwork installs no network or Web APIs.
type NoNetwork struct{}

// Install does nothing.
func (NoNetwork) Install(*Env) (func() error, error) { return nil, nil }

// Env is what a Feature sees while it installs: the runtime, its event loop
// and the helper methods of the high-level API. All methods run on the
// runtime's goroutine.
type Env struct {
	r   *Runtime
	cfg Config
}

// Config returns a copy of the configuration that created the runtime.
func (e *Env) Config() Config { return e.cfg }

// Runtime returns the canopy runtime being installed.
func (e *Env) Runtime() *Runtime { return e.r }

// Raw returns the underlying moejs runtime.
func (e *Env) Raw() *moejs.Runtime { return e.r.raw }

// Loop returns the runtime's event loop.
func (e *Env) Loop() *eventloop.Loop { return e.r.loop }

// Set installs a global value in the runtime, with the same conversion as
// Runtime.Set.
func (e *Env) Set(name string, v any) error { return e.r.Set(name, v) }

// SetFunc installs a global Go function, with the same conversion as
// Runtime.SetFunc.
func (e *Env) SetFunc(name string, fn any) error { return e.r.SetFunc(name, fn) }

// SetNamespace installs a global object from a map, with the same conversion
// as Runtime.SetNamespace.
func (e *Env) SetNamespace(name string, ns map[string]any) error {
	return e.r.SetNamespace(name, ns)
}

// consoleFeature installs a console global that writes to one writer.
type consoleFeature struct {
	cw *consoleWriter
}

func (c consoleFeature) Install(env *Env) (func() error, error) {
	if c.cw == nil {
		return nil, nil
	}
	return nil, c.cw.install(env)
}

// consoleWriter serializes writes from the pooled runtimes that share one
// console.
type consoleWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (c *consoleWriter) write(b []byte) {
	if c.w == nil || len(b) == 0 {
		return
	}
	c.mu.Lock()
	_, _ = c.w.Write(b)
	c.mu.Unlock()
}
