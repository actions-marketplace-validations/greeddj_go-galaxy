package collections

import "sync"

// apiRootMemo caches, per server base URL, the API root variant (v3, v2, or
// bare) that most recently answered a root-metadata request successfully.
// A real Ansible Galaxy server exposes exactly one API version at a stable
// root, so once a base's winning apiRoot is known, later collections on the
// same base can skip probing the losing variants instead of re-discovering
// them (and re-eating their 404s) on every single collection.
//
// The memo is scoped to one resolve/install/prefetch phase - it is created
// fresh per collectionDeps and is never persisted to the snapshot store: it
// is a purely in-process, per-run optimization, not durable state.
type apiRootMemo struct {
	winners map[string]string // server base -> winning apiRoot
	mu      sync.RWMutex
}

// newAPIRootMemo returns an empty memo. The map is presized to 1: a single
// Galaxy server for the whole run is the overwhelmingly common case.
func newAPIRootMemo() *apiRootMemo {
	return &apiRootMemo{winners: make(map[string]string, 1)}
}

// winner returns the previously recorded winning apiRoot for base, if any.
// A nil receiver (a collectionDeps built without newCollectionDeps, as some
// tests do) is treated as "no winner known yet" rather than panicking, so
// callers can pass deps.apiRoots through unconditionally.
func (m *apiRootMemo) winner(base string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	apiRoot, ok := m.winners[base]
	return apiRoot, ok
}

// recordWinner records apiRoot as the winning API root for base. Callers
// must call this only after a successful fetch, never on a 404: a single
// collection being absent under an apiRoot must not blacklist that apiRoot
// for the rest of the server's collections.
func (m *apiRootMemo) recordWinner(base, apiRoot string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.winners[base] = apiRoot
}
