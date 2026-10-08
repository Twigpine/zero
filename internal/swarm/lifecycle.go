package swarm

import (
	"context"
	"fmt"
	"time"
)

// Spawn registers a task and launches a member of agentType to run it under the
// given team, inheriting the orchestrator's policy. It returns the task id
// immediately; the member runs concurrently (under the Swarm's base context, so
// it outlives this call) and reports its result through the coordinator. If the
// team is at its slot cap the member is queued and launches when a slot frees
// (it stays pending in the coordinator until then).
func (s *Swarm) Spawn(pol Policy, teamName, agentType, task, cwd string) (string, error) {
	release, err := s.beginLifecycleAdmission()
	if err != nil {
		return "", err
	}
	defer release()

	def, err := s.registry.Lookup(agentType)
	if err != nil {
		return "", err
	}
	team := sanitizeName(teamName)
	id := s.nextID(agentType)
	if _, err := s.coord.Register(id, id, team, task); err != nil {
		return "", err
	}
	s.rememberCwd(id, cwd)
	s.startTaskRun(id)
	spec := s.buildSpec(pol, id, id, team, def, task, cwd)
	s.dispatchAdmitted(spec)
	return id, nil
}

// beginLifecycleAdmission admits one unit of lifecycle work if shutdown has not
// begun. On success it returns a release func the caller MUST call exactly once
// (a deferred call is the norm) — Close blocks until every admitted unit has
// released. On shutdown it returns a nil func and ErrSwarmClosed, and there is
// nothing to release.
//
// It deliberately does NOT keep lifecycleMu held for the caller's duration, which
// an earlier revision did. Close needs the write lock before it can cancel, so a
// launcher that blocks in Launch until its context is cancelled would deadlock
// against every Close caller: the launch holds the read lock while Close waits
// for the write lock to issue the cancellation neither side can reach. Holding the
// lock only across the flag check and the counter increment keeps the ordering
// guarantee (no admission once closed is set) without letting external launcher
// code sit inside the critical section.
//
// The read lock is enough to make the WaitGroup safe: Close flips closed under
// the write lock, so an Add here either completes before Close acquires it (and is
// therefore visible to the later Wait) or observes closed and is refused.
func (s *Swarm) beginLifecycleAdmission() (func(), error) {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	if s.closed {
		return nil, ErrSwarmClosed
	}
	s.lifecycleWork.Add(1)
	return s.lifecycleWork.Done, nil
}

// dispatch admits a spec to its team (launching now or queuing for a slot).
// The caller must hold an admission ticket from beginLifecycleAdmission.
func (s *Swarm) dispatchAdmitted(spec MemberSpec) {
	t := s.team(spec.Team)
	if t.admit(spec) {
		s.launchAdmitted(t, spec)
	}
	// Otherwise the spec is queued; the coordinator task stays pending until a
	// slot frees and afterExit launches it.
}

// launch starts a member for spec and supervises it. A synchronous launch
// failure fails the task and frees the slot.
// The caller must hold an admission ticket from beginLifecycleAdmission.
func (s *Swarm) launchAdmitted(t *Team, spec MemberSpec) {
	run := s.ensureTaskRun(spec.TaskID)
	handle, err := s.launchMemberAdmitted(spec, run)
	if err != nil {
		_ = s.coord.Fail(spec.TaskID, "launch: "+err.Error())
		run.finish()
		if err == ErrSwarmClosed {
			t.releaseSlot()
			return
		}
		s.afterExitAdmitted(t)
		return
	}

	committed := s.commitLaunch(handle, run, func() {
		m := &Member{ID: spec.ID, AgentType: spec.AgentType, TaskID: spec.TaskID, handle: handle}
		t.addMember(m)
		_ = s.coord.SetStatus(spec.TaskID, StatusRunning)
		s.watchers.Add(1)
		go func() {
			defer s.watchers.Done()
			s.watch(t, m, spec, run)
		}()
	})
	if !committed {
		_ = s.coord.Fail(spec.TaskID, "launch: "+ErrSwarmClosed.Error())
		run.finish()
		s.lifecycleMu.RLock()
		closed := s.closed
		s.lifecycleMu.RUnlock()
		if closed {
			t.releaseSlot()
		} else {
			s.afterExitAdmitted(t)
		}
	}
}

// commitLaunch closes the window between MemberLauncher.Launch returning and the
// swarm taking ownership of the handle it produced. Launch is external code and
// therefore runs without lifecycleMu held, so Close may have won after
// launchMemberAdmitted's pre-check but before Launch returned. adopt runs under
// the read lock so the shutdown check and everything the caller does to take
// ownership (registration, status transition, watcher Add, handle swap) are
// atomic with respect to Close setting closed and beginning its waits.
//
// It reports whether the handle was adopted. When shutdown or task cancellation
// won, the handle is reaped before returning false: MemberHandle has no separate
// cancellation operation — Launch's context is its cancellation contract — so
// waiting is how a successfully-created process/goroutine avoids being
// abandoned. The caller records the terminal outcome for the task it was
// launching.
func (s *Swarm) commitLaunch(handle MemberHandle, run *taskRun, adopt func()) bool {
	s.lifecycleMu.RLock()
	if s.closed || run.stopped() {
		s.lifecycleMu.RUnlock()
		_, _ = handle.Wait()
		return false
	}
	defer s.lifecycleMu.RUnlock()
	adopt()
	return true
}

// launchMemberAdmitted re-checks shutdown at the last internal boundary before
// invoking the external launcher. A lifecycle ticket proves the operation won
// admission before Close, but the caller may not reach its launch until after
// Close has set closed; in that case no new member should be started.
func (s *Swarm) launchMemberAdmitted(spec MemberSpec, run *taskRun) (MemberHandle, error) {
	s.lifecycleMu.RLock()
	closed := s.closed
	s.lifecycleMu.RUnlock()
	if closed {
		return nil, ErrSwarmClosed
	}
	if run.stopped() {
		return nil, context.Canceled
	}
	return s.launcher.Launch(run.ctx, spec)
}

// watch awaits a member, applies bounded relaunch on temporary failures, records
// the terminal outcome, then frees the slot and drains the queue.
//
// The relaunch counter is per-member (m.restarts) and the same spec is reused on
// retry. This is sound because a Member is bound 1:1 to its spec for its whole
// life (Member.ID == MemberSpec.ID); a retry never reuses the struct for a
// different spec.
func (s *Swarm) watch(t *Team, m *Member, spec MemberSpec, run *taskRun) {
	for {
		res, err := m.handle.Wait()
		if err != nil {
			if isRetryable(err) && m.restarts < maxMemberRestarts && s.relaunchAdmitted(m, spec, run) {
				continue
			}
			// Fall through if shutdown started or the relaunch failed.
			// res.SessionID is preserved by the handle even on error, so a member that
			// ran then failed stays drillable; a pure launch error carries an empty id.
			_ = s.coord.FailWithSession(m.TaskID, memberError(err), res.SessionID)
		} else {
			_ = s.coord.CompleteWithSession(m.TaskID, res.Result, res.SessionID)
		}
		break
	}
	t.removeMember(m.ID)
	// Publish the execution barrier before draining the team queue. Queue drain
	// invokes the external launcher synchronously and may block; the completed
	// source must not keep a handoff waiting on unrelated successor work.
	run.finish()
	s.afterExit(t)
}

// relaunchAdmitted performs one bounded relaunch of an existing member's spec,
// reporting whether the member was relaunched and its new handle adopted. A
// relaunch is lifecycle work like any other, so it runs the same three gates an
// initial launch does: admission (refused outright once shutdown began), the
// pre-Launch re-check, and the post-Launch commit — the last of which matters
// because the production FuncLauncher returns a live handle immediately without
// consulting its context, so a Close landing between the pre-check and the return
// would otherwise resume supervising a member started after shutdown. When any
// gate refuses, the caller records the member's terminal failure instead.
func (s *Swarm) relaunchAdmitted(m *Member, spec MemberSpec, run *taskRun) bool {
	release, err := s.beginLifecycleAdmission()
	if err != nil {
		return false
	}
	defer release()
	handle, err := s.launchMemberAdmitted(spec, run)
	if err != nil {
		return false
	}
	return s.commitLaunch(handle, run, func() {
		m.restarts++
		m.handle = handle
	})
}

// afterExit releases the just-vacated slot and launches the next queued spec, if
// any. Each exit drains at most one queued member; that member's own exit drains
// the next, so the queue empties one-per-slot without unbounded recursion.
func (s *Swarm) afterExit(t *Team) {
	release, err := s.beginLifecycleAdmission()
	if err != nil {
		t.releaseSlot()
		return
	}
	defer release()
	s.afterExitAdmitted(t)
}

// afterExitAdmitted drains at most one queued spec while lifecycle admission is
// held open. The caller must hold an admission ticket from beginLifecycleAdmission.
func (s *Swarm) afterExitAdmitted(t *Team) {
	next, ok := t.onExit()
	if !ok {
		return
	}
	// This call's own ticket only proves shutdown hadn't begun when it was
	// granted; Close may have set closed in the meantime, and t.onExit above
	// already removed next from t.queue, so Close's clearQueue sweep can no
	// longer see it (nor will it wait for one, since this ticket is what Close's
	// lifecycleWork.Wait() is blocked on). Re-check before launching so a dequeue
	// that straddles the shutdown boundary fails the task in place of a launch a
	// shutting-down swarm never wanted started.
	s.lifecycleMu.RLock()
	closed := s.closed
	s.lifecycleMu.RUnlock()
	if closed {
		t.releaseSlot()
		_ = s.coord.Fail(next.TaskID, ErrSwarmClosed.Error())
		s.finishTaskRun(next.TaskID)
		return
	}
	s.launchAdmitted(t, next)
}

// Handoff transfers a task to a fresh member of toAgentType, delivering a note to
// the new member's inbox and marking the original task handed-off. It cancels and
// joins the source task before dispatching the successor, so only one member can
// execute the objective at a time. It returns the new task id. A handoff of an
// already-terminal task is rejected (fail closed).
func (s *Swarm) Handoff(pol Policy, teamName, taskID, toAgentType, note string) (string, error) {
	release, err := s.beginLifecycleAdmission()
	if err != nil {
		return "", err
	}
	defer release()

	task, ok := s.coord.Get(taskID)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownTask, taskID)
	}
	if task.Status.terminal() {
		return "", fmt.Errorf("swarm: task %s already %s; cannot hand off", taskID, task.Status)
	}
	def, err := s.registry.Lookup(toAgentType)
	if err != nil {
		return "", err
	}
	team := sanitizeName(teamName)
	if task.Team != team {
		return "", fmt.Errorf("swarm: task %s belongs to team %s, not %s", taskID, task.Team, team)
	}
	task, err = s.coord.BeginHandoff(taskID)
	if err != nil {
		return "", err
	}
	run := s.taskRun(taskID)
	if run == nil {
		s.coord.AbortHandoff(taskID)
		return "", fmt.Errorf("swarm: task %s has no local execution boundary; refusing handoff", taskID)
	}
	newID := s.nextID(toAgentType)
	if err := s.coord.ReserveHandoffSuccessor(taskID, newID); err != nil {
		s.coord.AbortHandoff(taskID)
		return "", fmt.Errorf("swarm: reserve handoff successor: %w", err)
	}
	handoffTask := task.Description
	if note != "" {
		handoffTask += "\n\nHandoff note: " + note
	}
	// Deliver the handoff note to the new member's inbox BEFORE registering the new
	// task, so a mailbox failure can't leave a phantom pending task in the
	// coordinator (it returns the error having registered nothing) (M5).
	if note != "" {
		if mbErr := s.mailbox.Send(team, newID, Message{
			From: task.AgentID, Subject: "handoff", Body: note, Type: "handoff", Time: nowRFC3339(),
		}); mbErr != nil {
			s.coord.AbortHandoff(taskID)
			return "", fmt.Errorf("swarm: deliver handoff note: %w", mbErr)
		}
	}
	cwd := s.cwdFor(taskID)
	// Stop a running member, remove a queued one, or cancel a launch currently
	// between slot reservation and handle adoption. The completion barrier closes
	// only after no source member can execute further side effects.
	run.stop()
	if s.team(team).removeQueuedTask(taskID) {
		run.finish()
	}
	if err := s.waitForHandoffSource(taskID, run); err != nil {
		s.coord.AbortHandoff(taskID)
		// If the execution barrier raced the cancellation/timeout, its watcher may
		// have tried to finish while the claim was still held. Fail the stopped
		// source rather than leave a permanently-running coordinator record.
		if run.finished() {
			_ = s.coord.Fail(taskID, "handoff aborted after source stopped: "+err.Error())
		}
		return "", err
	}
	if _, err := s.coord.CommitHandoff(taskID, newID, newID, team, handoffTask); err != nil {
		s.coord.AbortHandoff(taskID)
		failErr := s.coord.Fail(taskID, "handoff successor registration failed after source stopped: "+err.Error())
		if failErr != nil {
			return "", fmt.Errorf("swarm: register handoff successor: %w (source recovery failed: %v)", err, failErr)
		}
		return "", fmt.Errorf("swarm: register handoff successor: %w", err)
	}
	s.rememberCwd(newID, cwd)
	s.startTaskRun(newID)
	spec := s.buildSpec(pol, newID, newID, team, def, handoffTask, cwd)
	s.dispatchAdmitted(spec)
	return newID, nil
}

// waitForHandoffSource waits for the task-specific execution barrier without
// letting a cancellation-insensitive member wedge a model tool forever. Close
// cancels baseCtx before waiting for lifecycle admissions, so that path also
// releases Handoff's admission ticket and avoids a shutdown wait cycle.
func (s *Swarm) waitForHandoffSource(taskID string, run *taskRun) error {
	timeout := s.handoffStopTimeout
	if timeout <= 0 {
		timeout = defaultHandoffStopTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-run.done:
		return nil
	case <-s.baseCtx.Done():
		// Prefer a completed barrier if cancellation and member exit became ready
		// together; a stopped source can still commit safely.
		if run.finished() {
			return nil
		}
		return fmt.Errorf("swarm: stop handoff source %s: %w", taskID, s.baseCtx.Err())
	case <-timer.C:
		if run.finished() {
			return nil
		}
		return fmt.Errorf("%w: task %s after %s", ErrHandoffStopTimeout, taskID, timeout)
	}
}

// AdoptOrphans re-parents tasks in a team whose owning member is no longer live
// (e.g. a crashed worker) onto fresh members of toAgentType, returning the
// adopted task ids. Terminal tasks and tasks with a live owner are left alone.
func (s *Swarm) AdoptOrphans(pol Policy, teamName, toAgentType string) ([]string, error) {
	release, err := s.beginLifecycleAdmission()
	if err != nil {
		return nil, err
	}
	defer release()

	def, err := s.registry.Lookup(toAgentType)
	if err != nil {
		return nil, err
	}
	team := sanitizeName(teamName)
	t := s.team(team)
	live := t.liveAgents()
	var adopted []string
	for _, task := range s.coord.List() {
		if task.Team != team || task.Status.terminal() {
			continue
		}
		if _, ok := live[task.AgentID]; ok {
			continue // still has a live member
		}
		newAgent := s.nextID(toAgentType)
		if err := s.coord.Reassign(task.ID, newAgent); err != nil {
			continue // raced to terminal; skip
		}
		cwd := s.cwdFor(task.ID)
		s.rememberCwd(task.ID, cwd)
		s.startTaskRun(task.ID)
		spec := s.buildSpec(pol, newAgent, task.ID, team, def, task.Description, cwd)
		s.dispatchAdmitted(spec)
		adopted = append(adopted, task.ID)
	}
	return adopted, nil
}

// Collect returns snapshots of every task in a team, for swarm_collect/status.
func (s *Swarm) Collect(teamName string) []Task {
	team := sanitizeName(teamName)
	var out []Task
	for _, task := range s.coord.List() {
		if task.Team == team {
			out = append(out, task)
		}
	}
	return out
}

// CollectWait blocks until the team's tasks all reach a terminal state (or ctx is
// done), then returns their snapshots. This is what swarm_collect uses so the
// orchestrator gets final results in a single call instead of polling
// swarm_status repeatedly while members are still running.
func (s *Swarm) CollectWait(ctx context.Context, teamName string) []Task {
	return s.coord.WaitSettled(ctx, sanitizeName(teamName))
}
