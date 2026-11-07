package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"sync"
	"time"
)

const (
	defaultMaxProcesses   = 64
	maxPendingOutputBytes = 2 * 1024 * 1024
	recentOutputBytes     = 4096
	processStopTimeout    = 3 * time.Second
	maxInteractiveYield   = 30 * time.Second
	maxEmptyPollYield     = 5 * time.Minute

	// defaultCompletedRetention is how long a finished process stays addressable.
	//
	// DERIVED FROM THE POLL BOUND, never chosen beside it. It was 30 seconds
	// while maxEmptyPollYield allowed a five-minute poll, and those two numbers
	// contradict each other: a caller Zero itself invites to wait five minutes
	// could arrive to find the answer already forgotten. In a measured run a
	// 60-second test was started, polled with yield_time_ms 40000, and the id was
	// gone — so the same 60-second test was run a second time to recover a result
	// the first one had already produced.
	//
	// Doubled so a caller that waits the full poll still has as long again to come
	// back with what it learned. TestCompletedRetentionCoversTheLongestPoll pins
	// the relationship rather than the number.
	defaultCompletedRetention = 2 * maxEmptyPollYield

	// maxRememberedCompletions bounds the finished-session records kept after a
	// process is evicted. Each immutable result is already bounded by the process
	// output cap; the count bound prevents a long session from accumulating them
	// without limit.
	maxRememberedCompletions = 64
)

var (
	ErrProcessNotFound      = errors.New("execution process not found")
	ErrProcessStdinDisabled = errors.New("execution process does not accept stdin")
	ErrProcessCapacity      = errors.New("execution process capacity reached; stop a running process before starting another")
)

type ProcessManagerOptions struct {
	CompletedRetention time.Duration
	// MaxProcesses bounds retained processes and in-flight launches. Completed
	// history is pruned before rejecting a start; live processes are never evicted.
	// Zero uses the default limit; a negative value disables the limit.
	MaxProcesses int
}

// ProcessManager owns retained interactive-process identity, transport,
// bounded output, continuation, cancellation, completion, and cleanup.
// completedProcess is the immutable terminal result a finished session leaves
// behind once its process is gone.
//
// A LATE POLL IS NOT A GUESS. Without this, an id that ran and finished is
// indistinguishable from one a model invented, and both are answered by
// UnknownExecSessionError — which tells the caller not to probe ids, having just
// handed it that id and instructed it to poll. The result is a re-run of work
// already done.
type completedProcess struct {
	result ProcessResult
}

type ProcessManager struct {
	mu                 sync.Mutex
	nextID             int
	processes          map[int]*managedProcess
	starting           int
	completedRetention time.Duration
	maxProcesses       int
	startTransport     processTransportStarter
	// completed remembers finished sessions after their process is evicted, in
	// arrival order so the oldest is dropped first.
	completed      map[int]completedProcess
	completedOrder []int
}

type ProcessStart struct {
	Prepared    PreparedCommand
	Request     Request
	CommandText string
	RelativeCwd string
	TTY         bool
	Metadata    map[string]string
	// AfterWait may append adapter diagnostics collected by a platform monitor.
	AfterWait func() []byte
}

type ProcessContinue struct {
	ProcessID int
	Input     []byte
	Interrupt bool
	Wait      time.Duration
}

type ProcessResult struct {
	ProcessID       int
	CommandText     string
	RelativeCwd     string
	TTY             bool
	Output          string
	OutputTruncated bool
	Exited          bool
	ExitCode        int
	Interrupted     bool
	Request         Request
	Enforcement     Enforcement
	Report          AdapterReport
	ReportErr       error
	Changes         []Change
	Metadata        map[string]string
}

type ProcessSnapshot struct {
	ID              int
	Command         string
	Cwd             string
	RelativeCwd     string
	StartedAt       time.Time
	LastUsedAt      time.Time
	TTY             bool
	Status          string
	ExitCode        *int
	RecentOutput    string
	OutputTruncated bool
}

func NewProcessManager(options ProcessManagerOptions) *ProcessManager {
	retention := options.CompletedRetention
	if retention == 0 {
		retention = defaultCompletedRetention
	}
	maxProcesses := options.MaxProcesses
	if maxProcesses == 0 {
		maxProcesses = defaultMaxProcesses
	}
	return &ProcessManager{
		nextID:             1000,
		processes:          make(map[int]*managedProcess),
		completed:          make(map[int]completedProcess),
		completedRetention: retention,
		maxProcesses:       maxProcesses,
		startTransport:     startProcessTransport,
	}
}

func (manager *ProcessManager) Start(ctx context.Context, input ProcessStart, wait time.Duration) (ProcessResult, error) {
	if manager == nil {
		return ProcessResult{}, errors.New("execution process manager is not configured")
	}
	if err := input.Request.Validate(); err != nil {
		return ProcessResult{}, fmt.Errorf("invalid execution request: %w", err)
	}
	if input.Prepared.Command == nil {
		return ProcessResult{}, errors.New("prepared execution has no command")
	}
	if err := manager.reserve(); err != nil {
		if input.Prepared.Cleanup != nil {
			input.Prepared.Cleanup()
		}
		return ProcessResult{}, err
	}
	command := input.Prepared.Command
	buffer := newProcessOutputBuffer()
	request := input.Request
	observer := NewChangeObserver(request.WorkspaceRoots[0])
	stdin, tty, transportCleanup, err := manager.startTransport(command, buffer, input.TTY)
	if err != nil {
		manager.mu.Lock()
		manager.starting--
		manager.mu.Unlock()
		if input.Prepared.Cleanup != nil {
			input.Prepared.Cleanup()
		}
		return ProcessResult{}, err
	}
	if tty {
		request.Mode = ModeInteractive
	} else {
		request.Mode = ModeCaptured
	}
	now := time.Now()
	process := &managedProcess{
		id:          manager.allocateID(),
		commandText: input.CommandText,
		cwd:         request.WorkingDirectory,
		relativeCwd: input.RelativeCwd,
		startedAt:   now,
		lastUsedAt:  now,
		tty:         tty,
		command:     command,
		request:     request,
		enforcement: input.Prepared.Enforcement,
		report:      input.Prepared.Report,
		cleanup:     input.Prepared.Cleanup,
		stdin:       stdin,
		output:      buffer,
		reaped:      make(chan struct{}),
		done:        make(chan struct{}),
		kill:        KillProcessTree,
		metadata:    cloneStringMap(input.Metadata),
	}
	manager.store(process)
	manager.removeCompletedLater(process)
	go func() {
		waitErr := command.Wait()
		close(process.reaped)
		if input.AfterWait != nil {
			if diagnostic := input.AfterWait(); len(diagnostic) > 0 {
				_, _ = buffer.Write(diagnostic)
			}
		}
		if transportCleanup != nil {
			transportCleanup()
		}
		adapterReport, reportErr := AdapterReport{}, error(nil)
		if process.report != nil {
			adapterReport, reportErr = process.report()
		}
		changes := observer.Changes()
		if process.cleanup != nil {
			process.cleanup()
		}
		process.markDone(waitErr, commandExitCode(waitErr), adapterReport, reportErr, changes)
	}()
	result := process.collectResult(ctx, clampInitialProcessWait(wait), false)
	if ctx != nil && ctx.Err() != nil && !result.Exited {
		process.terminate()
		more := process.collectResult(context.Background(), time.Second, true)
		var mergeTruncated bool
		result.Output, mergeTruncated = appendBoundedProcessOutputString(result.Output, more.Output)
		result.OutputTruncated = result.OutputTruncated || more.OutputTruncated
		result.OutputTruncated = result.OutputTruncated || mergeTruncated
		result.Exited = more.Exited
		result.ExitCode = more.ExitCode
		result.Interrupted = result.Interrupted || more.Interrupted
		result.Report = more.Report
		result.ReportErr = more.ReportErr
		result.Changes = more.Changes
	}
	if result.Exited {
		manager.remember(result)
		manager.Remove(process.id)
	}
	return result, nil
}

func (manager *ProcessManager) Continue(ctx context.Context, input ProcessContinue) (ProcessResult, error) {
	process, ok := manager.get(input.ProcessID)
	if !ok {
		// A SESSION THAT RAN AND FINISHED IS NOT A GUESSED ID. Answering both with
		// ErrProcessNotFound told a caller "do not probe session ids" about an id
		// this manager had issued and instructed it to poll — and threw away the
		// result, so the work was done again. An id never issued still falls
		// through to the error, so a real probe is still refused.
		if finished, remembered := manager.Completed(input.ProcessID); remembered {
			return finished, nil
		}
		return ProcessResult{}, ErrProcessNotFound
	}
	process.touch()
	if input.Interrupt {
		process.terminate()
	} else if len(input.Input) > 0 {
		if !process.tty || process.stdin == nil {
			return ProcessResult{}, ErrProcessStdinDisabled
		}
		if _, err := process.stdin.Write(input.Input); err != nil && !process.doneClosed() {
			return ProcessResult{}, err
		}
	}
	result := process.collectResult(ctx, clampContinuationWait(input.Wait, len(input.Input) == 0), input.Interrupt)
	if result.Exited {
		manager.remember(result)
		manager.Remove(process.id)
	}
	return result, nil
}

// WriteInput writes bytes to a retained interactive process's stdin without
// collecting output. Unlike Continue it never drains the pending output
// buffer, so a concurrent write_stdin poll still sees everything the process
// emitted; callers that only need the rolling tail use Snapshot instead.
func (manager *ProcessManager) WriteInput(id int, data []byte) error {
	process, ok := manager.get(id)
	if !ok {
		return ErrProcessNotFound
	}
	process.touch()
	if len(data) == 0 {
		return nil
	}
	if !process.tty || process.stdin == nil {
		return ErrProcessStdinDisabled
	}
	if _, err := process.stdin.Write(data); err != nil && !process.doneClosed() {
		return err
	}
	return nil
}

// ResizeInput updates the PTY window size of a retained interactive process
// so an attached terminal can fill its viewport. Non-positive dimensions are
// a no-op; platforms without PTY support report the transport's error.
func (manager *ProcessManager) ResizeInput(id int, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	process, ok := manager.get(id)
	if !ok {
		return ErrProcessNotFound
	}
	process.touch()
	if !process.tty || process.stdin == nil {
		return ErrProcessStdinDisabled
	}
	if err := resizePTY(process.stdin, cols, rows); err != nil && !process.doneClosed() {
		return err
	}
	return nil
}

func clampInitialProcessWait(wait time.Duration) time.Duration {
	return min(wait, maxInteractiveYield)
}

func clampContinuationWait(wait time.Duration, emptyPoll bool) time.Duration {
	if emptyPoll {
		return min(wait, maxEmptyPollYield)
	}
	return min(wait, maxInteractiveYield)
}

func (manager *ProcessManager) List() []ProcessSnapshot {
	manager.mu.Lock()
	processes := make([]*managedProcess, 0, len(manager.processes))
	for _, process := range manager.processes {
		if !process.doneClosed() {
			processes = append(processes, process)
		}
	}
	manager.mu.Unlock()
	snapshots := make([]ProcessSnapshot, 0, len(processes))
	for _, process := range processes {
		snapshots = append(snapshots, process.snapshot())
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	return snapshots
}

func (manager *ProcessManager) Snapshot(id int) (ProcessSnapshot, bool) {
	process, ok := manager.get(id)
	if ok {
		return process.snapshot(), true
	}
	result, ok := manager.Completed(id)
	if !ok {
		return ProcessSnapshot{}, false
	}
	exitCode := result.ExitCode
	return ProcessSnapshot{
		ID:              result.ProcessID,
		Command:         result.CommandText,
		Cwd:             result.Request.WorkingDirectory,
		RelativeCwd:     result.RelativeCwd,
		TTY:             result.TTY,
		Status:          "exited",
		ExitCode:        &exitCode,
		RecentOutput:    result.Output,
		OutputTruncated: result.OutputTruncated,
	}, true
}

func (manager *ProcessManager) Stop(id int) bool {
	process, ok := manager.get(id)
	if !ok {
		return false
	}
	process.terminate()
	return true
}

func (manager *ProcessManager) StopAll() []int {
	manager.mu.Lock()
	processes := make([]*managedProcess, 0, len(manager.processes))
	for _, process := range manager.processes {
		if !process.doneClosed() {
			processes = append(processes, process)
		}
	}
	manager.mu.Unlock()
	ids := make([]int, 0, len(processes))
	for _, process := range processes {
		process.terminate()
		ids = append(ids, process.id)
	}
	waitForProcesses(processes, processStopTimeout)
	sort.Ints(ids)
	return ids
}

// Remove forgets completed history only. A live process must remain tracked
// until completion, even if a termination attempt fails.
func (manager *ProcessManager) Remove(id int) {
	manager.mu.Lock()
	if process, ok := manager.processes[id]; ok && process.doneClosed() {
		delete(manager.processes, id)
	}
	manager.mu.Unlock()
}

// remember records a finished session so a late poll gets its result rather than
// an error telling it not to probe ids.
func (manager *ProcessManager) remember(result ProcessResult) {
	if !result.Exited || result.ProcessID == 0 {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if _, seen := manager.completed[result.ProcessID]; seen {
		return
	} else {
		manager.completedOrder = append(manager.completedOrder, result.ProcessID)
		for len(manager.completedOrder) > maxRememberedCompletions {
			delete(manager.completed, manager.completedOrder[0])
			manager.completedOrder = manager.completedOrder[1:]
		}
	}
	manager.completed[result.ProcessID] = completedProcess{result: cloneProcessResult(result)}
}

// Completed reports a finished session's result when the id ran in this process
// and has since been evicted. A never-issued id is not found, so a genuine probe
// is still refused.
func (manager *ProcessManager) Completed(id int) (ProcessResult, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record, ok := manager.completed[id]
	if !ok {
		return ProcessResult{}, false
	}
	return cloneProcessResult(record.result), true
}

func (manager *ProcessManager) Len() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return len(manager.processes)
}

func (manager *ProcessManager) allocateID() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	id := manager.nextID
	manager.nextID++
	return id
}

func (manager *ProcessManager) get(id int) (*managedProcess, bool) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	process, ok := manager.processes[id]
	return process, ok
}

func (manager *ProcessManager) reserve() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.maxProcesses > 0 && len(manager.processes)+manager.starting >= manager.maxProcesses {
		completed := manager.processToPruneLocked()
		if completed == nil {
			return ErrProcessCapacity
		}
		delete(manager.processes, completed.id)
	}
	manager.starting++
	return nil
}

func (manager *ProcessManager) store(process *managedProcess) {
	manager.mu.Lock()
	manager.starting--
	manager.processes[process.id] = process
	manager.mu.Unlock()
}

func (manager *ProcessManager) processToPruneLocked() *managedProcess {
	var processes []*managedProcess
	for _, process := range manager.processes {
		processes = append(processes, process)
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].lastUsed().Before(processes[j].lastUsed()) })
	for _, process := range processes {
		if process.doneClosed() {
			return process
		}
	}
	return nil
}

func (manager *ProcessManager) removeCompletedLater(process *managedProcess) {
	go func() {
		<-process.done
		// CAPTURED BEFORE THE WAIT, because the common case is that NOBODY polled
		// while it ran: a caller starts a long command, goes away to do other
		// work, and comes back after it finished. Recording only on the polling
		// paths would leave exactly that case with nothing to hand back — which is
		// the case that was measured, and it cost a 60-second test being run twice.
		manager.remember(process.collectResult(context.Background(), 0, false))
		if manager.completedRetention > 0 {
			timer := time.NewTimer(manager.completedRetention)
			<-timer.C
		}
		manager.Remove(process.id)
	}()
}

type managedProcess struct {
	id           int
	commandText  string
	cwd          string
	relativeCwd  string
	startedAt    time.Time
	lastUsedAt   time.Time
	tty          bool
	command      *exec.Cmd
	request      Request
	enforcement  Enforcement
	report       func() (AdapterReport, error)
	cleanup      func()
	stdin        io.WriteCloser
	output       *processOutputBuffer
	reaped       chan struct{}
	doneOnce     sync.Once
	done         chan struct{}
	kill         func(int) error
	mu           sync.Mutex
	exitCode     *int
	waitErr      error
	resultReport AdapterReport
	reportErr    error
	changes      []Change
	metadata     map[string]string
	collectMu    sync.Mutex
	terminal     *ProcessResult
	interrupted  bool
}

func (process *managedProcess) markDone(err error, exitCode int, report AdapterReport, reportErr error, changes []Change) {
	process.mu.Lock()
	process.waitErr = err
	process.exitCode = &exitCode
	process.resultReport = report
	process.reportErr = reportErr
	process.changes = append([]Change(nil), changes...)
	process.mu.Unlock()
	process.doneOnce.Do(func() { close(process.done) })
}

func (process *managedProcess) collectResult(ctx context.Context, wait time.Duration, interrupted bool) ProcessResult {
	// Output collection is destructive, so exactly one collector owns the
	// transition to a terminal result. Once the process exits, every caller and
	// the retention goroutine receives a clone of the same immutable result.
	process.collectMu.Lock()
	defer process.collectMu.Unlock()
	if process.terminal != nil {
		return cloneProcessResult(*process.terminal)
	}
	output, truncated := process.collect(ctx, wait)
	process.mu.Lock()
	exitCode := 0
	exited := process.exitCode != nil
	if exited {
		exitCode = *process.exitCode
	}
	result := ProcessResult{
		ProcessID: process.id, CommandText: process.commandText, RelativeCwd: process.relativeCwd,
		TTY: process.tty, Output: output, OutputTruncated: truncated, Exited: exited,
		ExitCode: exitCode, Interrupted: interrupted || process.interrupted, Request: cloneRequest(process.request),
		Enforcement: process.enforcement, Report: process.resultReport, ReportErr: process.reportErr,
		Changes: append([]Change(nil), process.changes...), Metadata: cloneStringMap(process.metadata),
	}
	process.mu.Unlock()
	if result.Exited {
		terminal := cloneProcessResult(result)
		process.terminal = &terminal
	}
	return result
}

func (process *managedProcess) collect(ctx context.Context, wait time.Duration) (string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(wait)
	var output []byte
	truncated := false
	finish := func() (string, bool) {
		truncated = truncated || process.output.consumeTruncated()
		return string(output), truncated
	}
	for {
		if chunk := process.output.drain(); len(chunk) > 0 {
			var chunkTruncated bool
			output, chunkTruncated = appendBoundedProcessOutput(output, chunk)
			truncated = truncated || chunkTruncated
			truncated = truncated || process.output.consumeTruncated()
			if time.Now().After(deadline) {
				return finish()
			}
			continue
		}
		if process.doneClosed() {
			var chunkTruncated bool
			output, chunkTruncated = appendBoundedProcessOutput(output, process.output.drain())
			truncated = truncated || chunkTruncated
			return finish()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return finish()
		}
		timer := time.NewTimer(remaining)
		select {
		case <-process.output.notify:
		case <-process.done:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return finish()
		case <-timer.C:
			return finish()
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func appendBoundedProcessOutput(output []byte, chunk []byte) ([]byte, bool) {
	if len(chunk) == 0 {
		return output, false
	}
	if len(chunk) >= maxPendingOutputBytes {
		bounded := make([]byte, maxPendingOutputBytes)
		copy(bounded, chunk[len(chunk)-maxPendingOutputBytes:])
		return bounded, len(output) > 0 || len(chunk) > maxPendingOutputBytes
	}
	if len(output)+len(chunk) <= maxPendingOutputBytes {
		return append(output, chunk...), false
	}
	drop := len(output) + len(chunk) - maxPendingOutputBytes
	copy(output, output[drop:])
	output = output[:len(output)-drop]
	return append(output, chunk...), true
}

func appendBoundedProcessOutputString(output string, chunk string) (string, bool) {
	bounded, truncated := appendBoundedProcessOutput([]byte(output), []byte(chunk))
	return string(bounded), truncated
}

func (process *managedProcess) doneClosed() bool {
	select {
	case <-process.done:
		return true
	default:
		return false
	}
}

func (process *managedProcess) touch() {
	process.mu.Lock()
	process.lastUsedAt = time.Now()
	process.mu.Unlock()
}
func (process *managedProcess) lastUsed() time.Time {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.lastUsedAt
}
func (process *managedProcess) terminate() {
	process.mu.Lock()
	process.interrupted = true
	process.mu.Unlock()
	if process.reapedClosed() || process.command.Process == nil {
		return
	}
	kill := process.kill
	if kill == nil {
		kill = KillProcessTree
	}
	_ = kill(process.command.Process.Pid)
}

func (process *managedProcess) reapedClosed() bool {
	if process.reaped == nil {
		return false
	}
	select {
	case <-process.reaped:
		return true
	default:
		return false
	}
}

func (process *managedProcess) snapshot() ProcessSnapshot {
	process.mu.Lock()
	defer process.mu.Unlock()
	var exit *int
	if process.exitCode != nil {
		value := *process.exitCode
		exit = &value
	}
	status := "running"
	if exit != nil {
		status = "exited"
	}
	return ProcessSnapshot{ID: process.id, Command: process.commandText, Cwd: process.cwd, RelativeCwd: process.relativeCwd,
		StartedAt: process.startedAt, LastUsedAt: process.lastUsedAt, TTY: process.tty, Status: status,
		ExitCode: exit, RecentOutput: process.output.recentString(), OutputTruncated: process.output.peekTruncated()}
}

func waitForProcesses(processes []*managedProcess, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for _, process := range processes {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-process.done:
		case <-timer.C:
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

type processOutputBuffer struct {
	mu        sync.Mutex
	data      []byte
	recent    []byte
	truncated bool
	notify    chan struct{}
}

func newProcessOutputBuffer() *processOutputBuffer {
	return &processOutputBuffer{notify: make(chan struct{}, 1)}
}
func (buffer *processOutputBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	buffer.data = append(buffer.data, data...)
	if len(buffer.data) > maxPendingOutputBytes {
		buffer.data = buffer.data[len(buffer.data)-maxPendingOutputBytes:]
		buffer.truncated = true
	}
	buffer.recent = append(buffer.recent, data...)
	if len(buffer.recent) > recentOutputBytes {
		buffer.recent = buffer.recent[len(buffer.recent)-recentOutputBytes:]
	}
	buffer.mu.Unlock()
	select {
	case buffer.notify <- struct{}{}:
	default:
	}
	return len(data), nil
}
func (buffer *processOutputBuffer) drain() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	data := append([]byte(nil), buffer.data...)
	buffer.data = nil
	return data
}
func (buffer *processOutputBuffer) recentString() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.recent)
}
func (buffer *processOutputBuffer) consumeTruncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	value := buffer.truncated
	buffer.truncated = false
	return value
}
func (buffer *processOutputBuffer) peekTruncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneRequest(input Request) Request {
	output := input
	output.Command.Args = append([]string(nil), input.Command.Args...)
	output.Command.Env = append([]string(nil), input.Command.Env...)
	output.WorkspaceRoots = append([]string(nil), input.WorkspaceRoots...)
	output.Capabilities = append([]Capability(nil), input.Capabilities...)
	return output
}

func cloneProcessResult(input ProcessResult) ProcessResult {
	output := input
	output.Request = cloneRequest(input.Request)
	output.Changes = append([]Change(nil), input.Changes...)
	output.Metadata = cloneStringMap(input.Metadata)
	if input.Report.Denial != nil {
		denial := *input.Report.Denial
		output.Report.Denial = &denial
	}
	return output
}
