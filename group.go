package procman

// groupHandle is the platform group/container handle held by a Process for
// the duration of a generation. On Windows it is a Job Object; on Unix it is
// nil (the process group is kernel-managed via Setpgid).
//
// Implementations are goroutine-safe and idempotent for terminate/close.
type groupHandle interface {
	// terminate kills the whole group/container. Idempotent.
	terminate() error
	// close releases the handle. Idempotent; safe to call after terminate.
	close()
}