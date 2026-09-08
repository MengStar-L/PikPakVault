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
