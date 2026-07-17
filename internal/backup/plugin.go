// Package backup implements PAIM's backup subsystem: a SQLite-persisted job
// queue (the domain.BackupJob rows ARE the queue, making it restart-safe by
// construction) and a worker pool that uploads verified assets to configured
// backup destinations through pluggable BackupPlugins.
//
// The core never imports a concrete plugin. Plugins register a factory with a
// Registry (main.go wires the built-in localfs plugin); the Manager resolves a
// job's provider configuration to a plugin instance at run time.
//
// Dependency direction follows the architecture spec: backup depends on repo
// (through small interfaces defined here at the point of consumption) and on
// domain, never the reverse. Engines take interfaces so they are testable
// without SQLite or the filesystem where practical.
package backup

import "context"

// Capabilities describes what a backup plugin can do. The Manager consults it to
// decide whether to run a post-upload Verify and to reject files that exceed a
// plugin's maximum size before attempting an upload.
type Capabilities struct {
	// SupportsVerify reports whether Verify performs a meaningful integrity
	// comparison (rather than a no-op that always returns true).
	SupportsVerify bool
	// SupportsDelete reports whether Delete is implemented.
	SupportsDelete bool
	// SupportsResume reports whether an interrupted Upload can be resumed rather
	// than restarted from the beginning.
	SupportsResume bool
	// MaxFileSize is the largest single file (in bytes) the plugin accepts. Zero
	// means unlimited.
	MaxFileSize int64
}

// Plugin is a backup destination. Implementations copy a local file to a remote
// location, optionally verify it, and optionally delete it. Every method takes a
// context so long-running network or disk I/O honors cancellation.
//
// remoteRelPath is always a forward-slash relative path that identifies the
// object within the destination; plugins map it onto their own namespace (a
// directory tree for localfs, an object key for a cloud store, and so on).
type Plugin interface {
	// Name returns the stable plugin identifier (e.g. "localfs"). It must match
	// the PluginName recorded on the BackupProvider rows the plugin serves.
	Name() string

	// Initialize validates and applies the provider configuration (a JSON object
	// whose shape is plugin-specific). It is called once per provider before any
	// upload and may probe the destination (e.g. writability).
	Initialize(ctx context.Context, configJSON string) error

	// Authenticate establishes or refreshes any credentials the destination
	// requires. For destinations that need none (e.g. localfs) it is a no-op.
	Authenticate(ctx context.Context) error

	// Upload copies the file at localPath to remoteRelPath. progressFn, when
	// non-nil, is called periodically with the number of bytes transferred so far
	// and the total; it must be safe to call from the uploading goroutine.
	Upload(ctx context.Context, localPath, remoteRelPath string, progressFn func(bytesDone, bytesTotal int64)) error

	// Delete removes (per the plugin's data-safety policy) the object at
	// remoteRelPath. localfs performs a soft delete into a trash folder rather
	// than an irreversible removal.
	Delete(ctx context.Context, remoteRelPath string) error

	// Verify re-reads the remote object and reports whether it matches the local
	// file byte-for-byte. Plugins whose Capabilities.SupportsVerify is false may
	// return (true, nil) unconditionally; the Manager skips Verify for them.
	Verify(ctx context.Context, localPath, remoteRelPath string) (bool, error)

	// Capabilities reports the plugin's feature set.
	Capabilities() Capabilities
}

// Factory constructs a fresh, unconfigured Plugin. The Manager calls a factory
// once per provider and then Initializes the returned instance.
type Factory func() Plugin

// Registry maps plugin names to factories. The core registers no concrete
// plugins; main.go (or a test) registers the ones it wants available. A Registry
// is safe for concurrent reads after construction; registration is expected to
// happen during startup before the Manager runs.
type Registry struct {
	factories map[string]Factory
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register associates name with factory. Registering the same name twice
// overwrites the previous factory (last registration wins), which keeps startup
// wiring simple and deterministic.
func (r *Registry) Register(name string, factory Factory) {
	r.factories[name] = factory
}

// New constructs a fresh plugin instance for name. It returns (nil, false) when
// no plugin is registered under that name.
func (r *Registry) New(name string) (Plugin, bool) {
	f, ok := r.factories[name]
	if !ok {
		return nil, false
	}
	return f(), true
}

// Names returns the registered plugin names in no particular order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	return out
}
