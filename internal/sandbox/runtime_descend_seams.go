package sandbox

// The two seams the rooted descent exposes, shared by both platform
// implementations so a test asserts the same property on either.
//
// They live apart from the Windows descent they started in because the POSIX
// fallback now takes the same shape, and a seam defined next to one of two
// implementations is how the other one quietly ends up untested.

// runtimeDescentBarrier, when set, runs after the base directory has been opened
// and before the first owned component is touched. It exists so a test can swap
// an owned component for a link at exactly the point the old pathname walk was
// vulnerable, and prove the redirected target is never created or granted. Nil in
// production.
var runtimeDescentBarrier func()

// runtimeCreationFailure, when set, is consulted immediately after this run
// creates a runtime object and before the step that would publish it: a
// directory component's identity read, or the lease file's inspection, wrapping
// and locking. Returning an error stands in for that step failing.
//
// It is the only way to reach those paths, and they are the ones where the
// transaction knows it created something that nothing above it knows about yet.
// The argument is the created component's path, or the lease file's name. Nil in
// production.
var runtimeCreationFailure func(string) error

// runtimeCleanupExclusivityBarrier, when set, runs during compensation while the
// exclusive cleanup lease is held: after it has been taken and before the first
// thing is removed. It exists so a test can put a contender into that window and
// prove the exclusion still covers it, which is precisely where the old shape had
// already handed the lease back. Nil in production.
var runtimeCleanupExclusivityBarrier func()

// runtimeBaseOpenedByName, when set, receives the ONE path this descent opens by
// name. The whole security property is which path that is: the fixed cache or
// temp directory above the owned tail, never a predictable component Zero owns. A
// test can assert it directly instead of inferring it from whether a swap
// happened to be caught, which is not discriminating. Nil in production.
var runtimeBaseOpenedByName func(string)
