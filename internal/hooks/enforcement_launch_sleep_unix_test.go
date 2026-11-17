//go:build !windows

package hooks

// sleepScript keeps a launched child alive long enough for a timeout to fire.
const sleepScript = "sleep 2"
