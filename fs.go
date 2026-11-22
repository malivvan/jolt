package js

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/node"
	"github.com/MoeForge/moejs/webapi"
)

// HostFS is a file system feature backed by moejs's node:fs modules. Every
// path resolves inside Root through an os.Root, so neither ".." nor a
// symbolic link leaves it. The file system is read-only unless Write is set.
//
// Root must exist. JavaScript sees Root at Mount; relative paths resolve
// against Cwd.
type HostFS struct {
	// Root is the host directory that is the file system.
	Root string
	// Mount is the absolute path at which JavaScript sees Root, such as
	// "/workspace". Empty means Root itself.
	Mount string
	// Cwd is the directory relative paths resolve against. Empty means Mount.
	Cwd string
	// Write allows functions that change the file system. Without it they
	// fail with EROFS.
	Write bool
	// Allow, when not nil, approves every access. op is "read" or "write" and
	// path is the absolute path in JavaScript's view, with symbolic links
	// resolved.
	Allow func(op, path string) error
	// Workers bounds concurrent promise/callback file system operations.
	// Zero means 4.
	Workers int
	// Buffer installs the global Buffer and node:buffer.
	Buffer bool
	// NoDescriptorCache turns off the Linux directory descriptor cache. See
	// moejs/node.FSOptions.
	NoDescriptorCache bool
}

// NewHostFS returns a read-only HostFS rooted at root and mounted at root.
func NewHostFS(root string) *HostFS { return &HostFS{Root: root} }

// Install installs node:fs, node:fs/promises and optionally Buffer.
func (f *HostFS) Install(env *Env) (func() error, error) {
	api, err := node.InstallLoop(env.Loop(), node.Options{
		FS: &node.FSOptions{
			Root:              f.Root,
			Mount:             f.Mount,
			Cwd:               f.Cwd,
			Write:             f.Write,
			Allow:             f.Allow,
			Workers:           f.Workers,
			NoDescriptorCache: f.NoDescriptorCache,
		},
		Buffer: f.Buffer,
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		api.Close()
		return nil
	}, nil
}

// Resolver resolves node:fs, node:fs/promises and node:buffer.
func (f *HostFS) Resolver(next moejs.Resolver) moejs.Resolver {
	return node.Resolver(next)
}

// Importer returns an importer that serves the file system modules and
// passes everything else to next.
func (f *HostFS) Importer(next *moejs.Importer) *moejs.Importer {
	return node.Importer(next)
}

// HostNetwork installs fetch, Headers, Request, Response, the Streams, URL,
// AbortController, DOMException and performance, backed by an http.Client.
//
// With a nil Client the Web APIs are installed but fetch has no network: it
// rejects every request. Allow, when not nil, approves every request,
// redirects included; a nil Allow approves every address the client can
// reach, so set one for untrusted scripts.
type HostNetwork struct {
	// Client sends fetch requests. Nil means no network.
	Client *http.Client
	// Allow approves each request before it is sent.
	Allow func(*http.Request) error
	// Context is the parent of every request's context.
	Context context.Context
}

// Install installs the Web APIs.
func (n *HostNetwork) Install(env *Env) (func() error, error) {
	api, err := webapi.Install(env.Loop(), webapi.Options{
		Client:  n.Client,
		Allow:   n.Allow,
		Context: n.Context,
	})
	if err != nil {
		return nil, err
	}
	return func() error {
		api.Close()
		return nil
	}, nil
}

// AllowHosts returns an Allow hook that permits only the given hosts. A
// leading "*." matches a domain suffix, and a port in a pattern is ignored
// (the check is against the request's host name). The request's Host header is
// checked too when it differs from the URL.
func AllowHosts(hosts ...string) func(*http.Request) error {
	normalized := make([]string, len(hosts))
	for i, h := range hosts {
		normalized[i] = hostOnly(strings.TrimSpace(h))
	}
	return webapi.AllowHosts(normalized...)
}

func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		return h[1 : len(h)-1]
	}
	return h
}
