package vault

import "context"

// Only lives for one execution, never across a retry, scan or account switch.
// Local revisions are checked before reusing a resolved directory.
type operationKey struct{}
type resolvedFolder struct {
	id, parent string
	revision   int64
}
type operationCache struct {
	root, rootPath string
	folders        map[string]resolvedFolder
}

func operation(ctx context.Context) *operationCache {
	v, _ := ctx.Value(operationKey{}).(*operationCache)
	return v
}

type structureKey struct{}

// Recursive folder/root resolution and alignment share one short lock. The
// marker is private to an execution context, which is never shared by workers.
func (a *App) lockStructure(ctx context.Context) (context.Context, func()) {
	if ctx.Value(structureKey{}) == a {
		return ctx, func() {}
	}
	a.structureMu.Lock()
	return context.WithValue(ctx, structureKey{}, a), a.structureMu.Unlock
}
