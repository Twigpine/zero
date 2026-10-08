package swarm

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// TaskStatus is the lifecycle state of a swarm task.
type TaskStatus string

const (
	// StatusPending: registered but the member has not started.
	StatusPending TaskStatus = "pending"
	// StatusRunning: the member is executing.
	StatusRunning TaskStatus = "running"
	// StatusDone: the member finished successfully.
	StatusDone TaskStatus = "done"
	// StatusFailed: the member exited with an error.
	StatusFailed TaskStatus = "failed"
	// StatusHandedOff: ownership was transferred to another agent.
	StatusHandedOff TaskStatus = "handed-off"
)

// terminal reports whether a status is final (no further transitions expected).
func (s TaskStatus) terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusHandedOff
}

// valid reports whether a status is one of the known lifecycle states. Unknown
// statuses are rejected (fail closed) so a task can never enter an undefined
// state that Summarize and the lifecycle logic cannot reason about.
func (s TaskStatus) valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusDone, StatusFailed, StatusHandedOff:
		return true
	default:
		return false
	}
}

// Task is one unit of work tracked by the coordinator. It is returned by value
// from snapshots so callers cannot mutate coordinator state without the lock.
type Task struct {
	ID          string
	AgentID     string
	Team        string
	Description string
	Status      TaskStatus
	Result      string
	Err         string
	// SessionID is the member's durable child session id, recorded on completion
	// so the TUI can drill into the member's conversation. Empty until done.
	SessionID string
	CreatedAt time.Time
	UpdatedAt time.Time
	handoff   bool // internal claim; status stays non-terminal until the source exits
}

// agentColors palette for stable per-agent coloring in the TUI/status. Provider
// -neutral ANSI-ish names; the renderer maps them to styles.
var agentColors = []string{"cyan", "magenta", "green", "yellow", "blue", "red"}

// Coordinator is the in-memory task registry + team color assigner shared by an
// orchestrator and its members. It is safe for concurrent use.
type Coordinator struct {
	mu                  sync.RWMutex
	tasks               map[string]*Task
	handoffReservations map[string]string // successor task id -> claimed source task id
	colors              map[string]string // agentID -> color
	colorIndex          int
	now                 func() time.Time // injectable clock for tests
	// changed is closed (and replaced) on every task state change so WaitSettled
	// can block for a transition without polling. Always non-nil after construction.
	changed chan struct{}
}

// NewCoordinator returns an empty coordinator using the wall clock.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		tasks:               map[string]*Task{},
		handoffReservations: map[string]string{},
		colors:              map[string]string{},
		now:                 time.Now,
		changed:             make(chan struct{}),
	}
}

// notifyChangeLocked wakes every WaitSettled caller by closing the current change
// channel and installing a fresh one. Must be called while holding c.mu for
// writing; lazily initialises changed so a zero-value coordinator is still safe.
func (c *Coordinator) notifyChangeLocked() {
	if c.changed != nil {
		close(c.changed)
	}
	c.changed = make(chan struct{})
}

// ErrTaskExists is returned when registering a duplicate task ID.
var ErrTaskExists = errors.New("swarm: task already registered")

// ErrUnknownTask is returned when updating/removing a missing task ID.
var ErrUnknownTask = errors.New("swarm: unknown task")

// Register adds a new pending task. The ID must be unique and non-empty.
func (c *Coordinator) Register(id, agentID, team, description string) (Task, error) {
	if id == "" {
		return Task{}, errors.New("swarm: task id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tasks[id]; ok {
		return Task{}, fmt.Errorf("%w: %s", ErrTaskExists, id)
	}
	if _, reserved := c.handoffReservations[id]; reserved {
		return Task{}, fmt.Errorf("%w: %s is reserved for handoff", ErrTaskExists, id)
	}
	now := c.now()
	t := &Task{
		ID:          id,
		AgentID:     agentID,
		Team:        team,
		Description: description,
		Status:      StatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	c.tasks[id] = t
	c.assignColorLocked(agentID)
	c.notifyChangeLocked()
	return *t, nil
}

// SetStatus transitions a task. Transitions out of a terminal state are
// rejected (fail closed) so a late member update cannot resurrect a finished
// task.
func (c *Coordinator) SetStatus(id string, status TaskStatus) error {
	if !status.valid() {
		return fmt.Errorf("swarm: invalid task status %q", status)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	if t.Status.terminal() && t.Status != status {
		return fmt.Errorf("swarm: task %s is %s (terminal); cannot move to %s", id, t.Status, status)
	}
	if t.handoff {
		return fmt.Errorf("swarm: task %s is being handed off; cannot move to %s", id, status)
	}
	t.Status = status
	t.UpdatedAt = c.now()
	c.notifyChangeLocked()
	return nil
}

// BeginHandoff atomically claims a non-terminal task for one handoff caller
// without reporting it terminal. The visible status remains pending/running
// until CommitHandoff, so collectors cannot observe settled state while the
// source member is still unwinding.
func (c *Coordinator) BeginHandoff(id string) (Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	if t.Status.terminal() {
		return Task{}, fmt.Errorf("swarm: task %s already %s; cannot hand off", id, t.Status)
	}
	if t.handoff {
		return Task{}, fmt.Errorf("swarm: task %s is already being handed off", id)
	}
	t.handoff = true
	return *t, nil
}

// AbortHandoff releases a claim and any successor reservation when handoff
// preparation fails or a stopped source does not reach its completion barrier.
func (c *Coordinator) AbortHandoff(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.tasks[id]; ok && t.handoff && !t.Status.terminal() {
		t.handoff = false
	}
	for successorID, sourceID := range c.handoffReservations {
		if sourceID == id {
			delete(c.handoffReservations, successorID)
		}
	}
}

// ReserveHandoffSuccessor claims successorID for sourceID before any mailbox or
// cancellation side effect. Register rejects reserved IDs, so another Swarm
// sharing this Coordinator cannot steal the id between note delivery and the
// atomic handoff commit.
func (c *Coordinator) ReserveHandoffSuccessor(sourceID, successorID string) error {
	if successorID == "" {
		return errors.New("swarm: successor task id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	source, ok := c.tasks[sourceID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, sourceID)
	}
	if !source.handoff || source.Status.terminal() {
		return fmt.Errorf("swarm: task %s has no active handoff claim", sourceID)
	}
	if _, exists := c.tasks[successorID]; exists {
		return fmt.Errorf("%w: %s", ErrTaskExists, successorID)
	}
	if _, reserved := c.handoffReservations[successorID]; reserved {
		return fmt.Errorf("%w: %s is reserved for handoff", ErrTaskExists, successorID)
	}
	if c.handoffReservations == nil {
		c.handoffReservations = map[string]string{}
	}
	c.handoffReservations[successorID] = sourceID
	return nil
}

// CommitHandoff atomically registers the successor and publishes the source's
// handed-off terminal state. The successor is inserted first while c.mu keeps
// the intermediate state invisible, so an ID collision cannot retire the
// source and orphan adoption cannot observe a successor without its source
// transition committed.
func (c *Coordinator) CommitHandoff(sourceID, successorID, successorAgentID, team, description string) (Task, error) {
	if successorID == "" {
		return Task{}, errors.New("swarm: successor task id is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	source, ok := c.tasks[sourceID]
	if !ok {
		return Task{}, fmt.Errorf("%w: %s", ErrUnknownTask, sourceID)
	}
	if !source.handoff {
		return Task{}, fmt.Errorf("swarm: task %s has no handoff in progress", sourceID)
	}
	if source.Status.terminal() {
		return Task{}, fmt.Errorf("swarm: task %s already %s", sourceID, source.Status)
	}
	if reservedFor, ok := c.handoffReservations[successorID]; !ok || reservedFor != sourceID {
		return Task{}, fmt.Errorf("swarm: successor task %s is not reserved for handoff from %s", successorID, sourceID)
	}
	if _, exists := c.tasks[successorID]; exists {
		return Task{}, fmt.Errorf("%w: %s", ErrTaskExists, successorID)
	}

	now := c.now()
	successor := &Task{
		ID:          successorID,
		AgentID:     successorAgentID,
		Team:        team,
		Description: description,
		Status:      StatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	// Registration precedes retirement under the same lock. No reader can see
	// either half until both ownership records are valid.
	c.tasks[successorID] = successor
	delete(c.handoffReservations, successorID)
	c.assignColorLocked(successorAgentID)
	source.handoff = false
	source.Status = StatusHandedOff
	source.UpdatedAt = now
	c.notifyChangeLocked()
	return *successor, nil
}

// Complete marks a task done with its result.
func (c *Coordinator) Complete(id, result string) error {
	return c.finish(id, StatusDone, result, "", "")
}

// CompleteWithSession marks a task done with its result and the member's durable
// child session id (so the TUI can drill into the member's conversation).
func (c *Coordinator) CompleteWithSession(id, result, sessionID string) error {
	return c.finish(id, StatusDone, result, "", sessionID)
}

// Fail marks a task failed with an error message.
func (c *Coordinator) Fail(id, errMsg string) error {
	return c.finish(id, StatusFailed, "", errMsg, "")
}

// FailWithSession marks a task failed but keeps its child session id, so a member
// that ran and failed (e.g. exited non-zero / hit max-turns) is still drillable.
func (c *Coordinator) FailWithSession(id, errMsg, sessionID string) error {
	return c.finish(id, StatusFailed, "", errMsg, sessionID)
}

func (c *Coordinator) finish(id string, status TaskStatus, result, errMsg, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	if t.Status.terminal() {
		return fmt.Errorf("swarm: task %s already %s", id, t.Status)
	}
	if t.handoff {
		return fmt.Errorf("swarm: task %s is being handed off", id)
	}
	t.Status = status
	t.Result = result
	t.Err = errMsg
	if sessionID != "" {
		t.SessionID = sessionID
	}
	t.UpdatedAt = c.now()
	c.notifyChangeLocked()
	return nil
}

// Reassign transfers a task to a new agent (handoff/orphan-adoption). The task
// returns to pending under the new owner unless it is already terminal.
func (c *Coordinator) Reassign(id, newAgentID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tasks[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownTask, id)
	}
	if t.Status.terminal() {
		return fmt.Errorf("swarm: task %s already %s; cannot reassign", id, t.Status)
	}
	if t.handoff {
		return fmt.Errorf("swarm: task %s is being handed off; cannot reassign", id)
	}
	t.AgentID = newAgentID
	t.Status = StatusPending
	t.UpdatedAt = c.now()
	c.assignColorLocked(newAgentID)
	c.notifyChangeLocked()
	return nil
}

// Get returns a snapshot of one task.
func (c *Coordinator) Get(id string) (Task, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tasks[id]
	if !ok {
		return Task{}, false
	}
	return *t, true
}

// List returns snapshots of all tasks, ordered by creation time then ID for
// stable output.
func (c *Coordinator) List() []Task {
	c.mu.RLock()
	out := make([]Task, 0, len(c.tasks))
	for _, t := range c.tasks {
		out = append(out, *t)
	}
	c.mu.RUnlock()
	sortTasksByCreation(out)
	return out
}

// sortTasksByCreation orders tasks by creation time then ID for stable output.
func sortTasksByCreation(tasks []Task) {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].ID < tasks[j].ID
		}
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
}

// WaitSettled blocks until every task in the scope (one team, or all teams when
// team=="") has reached a terminal status, then returns a snapshot of the scope.
// It also returns — with the latest partial state — as soon as ctx is done, so a
// stuck or long-running member can't hang the caller forever (the caller bounds
// it with a timeout / user-cancel context). A scope with no tasks is settled
// immediately, so an empty or unknown team never blocks. This is the primitive
// that lets swarm_collect deliver final results in one call instead of forcing
// the orchestrator to poll swarm_status in a loop.
//
// The settled decision and the returned snapshot are taken in the SAME locked
// pass, so a concurrent Register/Reassign slipping in between cannot make the
// call return a non-terminal task it never actually waited on.
func (c *Coordinator) WaitSettled(ctx context.Context, team string) []Task {
	for {
		c.mu.RLock()
		settled := true
		snapshot := make([]Task, 0, len(c.tasks))
		for _, t := range c.tasks {
			if team != "" && t.Team != team {
				continue
			}
			snapshot = append(snapshot, *t)
			if !t.Status.terminal() {
				settled = false
			}
		}
		ch := c.changed
		c.mu.RUnlock()
		if settled || ch == nil {
			sortTasksByCreation(snapshot)
			return snapshot
		}
		select {
		case <-ctx.Done():
			sortTasksByCreation(snapshot)
			return snapshot
		case <-ch:
		}
	}
}

// Color returns the stable color assigned to an agent (assigning one on first
// use), so status output colors each member consistently.
func (c *Coordinator) Color(agentID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.assignColorLocked(agentID)
}

func (c *Coordinator) assignColorLocked(agentID string) string {
	if agentID == "" {
		return ""
	}
	if color, ok := c.colors[agentID]; ok {
		return color
	}
	color := agentColors[c.colorIndex%len(agentColors)]
	c.colors[agentID] = color
	c.colorIndex++
	return color
}

// Summary is an aggregate count of tasks by status for the status tool.
type Summary struct {
	Total     int
	Pending   int
	Running   int
	Done      int
	Failed    int
	HandedOff int
}

// Summarize aggregates current task counts.
func (c *Coordinator) Summarize() Summary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := Summary{Total: len(c.tasks)}
	for _, t := range c.tasks {
		switch t.Status {
		case StatusPending:
			s.Pending++
		case StatusRunning:
			s.Running++
		case StatusDone:
			s.Done++
		case StatusFailed:
			s.Failed++
		case StatusHandedOff:
			s.HandedOff++
		}
	}
	return s
}
