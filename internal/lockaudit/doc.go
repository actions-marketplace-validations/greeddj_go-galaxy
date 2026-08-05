// Package lockaudit gates one structural property of the source: that every
// command which takes the cache backend's exclusive lock runs its work under
// the HOLDER context that lock handed back, and judges the outcome against
// that same context through cacheManager.LockLostError.
//
// It holds no production code and nothing imports it: its entire content is
// tests, the same load-bearing choice internal/proseaudit makes. `go test
// ./...` already runs them, so the gate needs no Justfile target, no CI step,
// no new dependency and no depguard allow-list entry of its own: go/ast,
// go/parser and go/token are already allowed for the module's other
// source-auditing test package.
//
// The property is checked over a closed table of four lifecycle functions,
// and a table entry that names a function the file does not contain is a
// failure rather than a skip: a rename would otherwise turn the whole gate
// into a passing no-op, which is precisely the shape a gate must never take.
// See holder_test.go for the exact predicate.
//
// RESIDUAL, and it is the whole reason this is an AST gate rather than a
// test: it proves the source COMPOSES those calls, never that the composition
// BEHAVES at runtime. No unit test can drive runInstall (or its three
// siblings) with a backend capable of losing a lock - each reaches
// cacheBackend.New through the initInstall/initCleanup it calls, which
// constructs the backend itself, so there is no seam to inject one, and the
// local backend a test can actually drive returns the caller's own context as
// its holder and cannot lose a lock by construction. What this gate therefore
// catches is exactly the class of edit those functions are exposed to: a work
// call quietly handed the caller's context instead of the holder's, or a
// return that stops going through the verdict. What it cannot catch is a
// LockLostError whose own decision table is wrong, which is pinned instead by
// that function's own tests in internal/galaxy/cache.
//
// This must NOT be generalized into a context-threading linter. The rule it
// encodes is specific to one seam - a backend lock's holder context, four
// named functions, one verdict function - and is worth an AST gate only
// because that seam has no runtime test. A general "the right context is
// passed here" checker over the whole module would be a heuristic about
// ordinary Go style, would fire on correct code, and would have to grow
// exemptions until it stopped meaning anything.
package lockaudit
