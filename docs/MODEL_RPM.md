# Local per-model completion limits

Add `modelRPM` to the existing Zero config file:

```json
{"modelRPM": {"gpt-4.1": 30, "claude-sonnet-4.5": 15}}
```

Keys resolve through the model registry; custom IDs match exactly after trimming
whitespace. Missing keys and zero mean unlimited. Negative limits and conflicting
aliases are configuration errors. Project config may add or tighten a user cap,
but cannot relax it. Changes take effect on the next Zero process start.

For `zero exec` and interactive agent turns, admission happens immediately before
the wrapped turn session's `Stream`. N admissions fit in any sliding 60-second
window; N+1 returns a local rate-limit error before entering that method. The
error reports when a slot expires. Exec uses its existing provider-error exit
code plus a local-limit hint. There is no sleep or hidden retry. Failed admitted
requests still consume a slot.

Sessions and model switches share a limiter in one process. Separate processes
have independent windows; restarting clears them. This is not an account-wide
cap. It counts Stream admissions, not all HTTP requests: setup/prewarm,
compaction, provider-internal retries, discovery and direct calls outside that
boundary are not counted. No TPM, cost budgets, persistence, cross-process
coordination or new runtime/Python dependency is introduced.
