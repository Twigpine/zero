package sandbox

// runtimeLeasePreCreateBarrier, when set, runs after lease acquisition has
// decided what it is going to do and before it creates anything.
//
// It exists so a test can reproduce the CHECK-THEN-USE RACE rather than a
// junction that was already there when acquisition started. The two are not the
// same finding: a junction present from the outset was already refused by the
// alias pre-check, while one planted in this window defeated it, because
// os.MkdirAll and the lease open both resolved the component again and followed
// it. A test that only plants the junction up front passes against the defective
// implementation and proves nothing.
//
// Nil in production.
var runtimeLeasePreCreateBarrier func()
