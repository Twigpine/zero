package hooks

// sleepScript keeps a launched child alive long enough for a timeout to fire.
// Paired with the !windows file of the same name; both must exist or the
// platform without one silently loses the timeout case.
const sleepScript = "ping -n 3 127.0.0.1 > NUL"
