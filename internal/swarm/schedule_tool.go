package swarm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/tools"
)

// ScheduleToolName is the recurring-spawn scheduler tool.
const ScheduleToolName = "swarm_schedule"

// ---- swarm_schedule --------------------------------------------------------

type scheduleTool struct {
	sw  *Swarm
	now func() time.Time // injectable for tests; nil => time.Now
	deferredSwarmTool
}

func (t *scheduleTool) Name() string { return ScheduleToolName }
func (t *scheduleTool) Description() string {
	return "Manage recurring swarm spawns. action=add schedules an agent to spawn on an interval (every, e.g. \"30m\") or daily (daily_at \"HH:MM\"); action=list shows active schedules; action=cancel stops one by job_id. Each fire spawns a fresh member; a fire is skipped while the job's previous spawn is still running."
}

func (t *scheduleTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"action":      {Type: "string", Description: "add (default), list, or cancel."},
			"agent_type":  {Type: "string", Description: "add: roster agent type to spawn each fire."},
			"task":        {Type: "string", Description: "add: the task/briefing handed to each spawned member."},
			"team":        {Type: "string", Description: "Team to spawn into. Defaults to \"default\"."},
			"every":       {Type: "string", Description: "add: interval between fires as a Go duration (e.g. \"30m\", \"2h\"). Minimum 1s. Mutually exclusive with daily_at."},
			"daily_at":    {Type: "string", Description: "add: local time of day \"HH:MM\" to fire once per day. Mutually exclusive with every."},
			"first_delay": {Type: "string", Description: "add: optional delay before the first fire (Go duration). Ignored with daily_at."},
			"max_runs":    {Type: "integer", Description: "add: stop after this many successful spawns. 0 or omitted => unbounded."},
			"job_id":      {Type: "string", Description: "cancel: the schedule id to stop."},
		},
		Required:             []string{},
		AdditionalProperties: false,
	}
}

func (t *scheduleTool) Safety() tools.Safety {
	return tools.Safety{
		// Adding a schedule spawns members repeatedly over time, so the tool is
		// classified like swarm_spawn (prompts) even though list/cancel are light.
		SideEffect:      tools.SideEffectShell,
		Permission:      tools.PermissionPrompt,
		Reason:          "Schedules recurring swarm member spawns under the orchestrator's sandbox and policy.",
		AdvertiseInAuto: true,
	}
}

func (t *scheduleTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return t.RunWithOptions(ctx, args, tools.RunOptions{})
}

func (t *scheduleTool) RunWithOptions(_ context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	action := strings.ToLower(swarmStr(args, "action"))
	if action == "" {
		action = "add"
	}
	switch action {
	case "add":
		return t.add(args, options)
	case "list":
		return t.list()
	case "cancel":
		return t.cancel(args)
	default:
		return errResult("swarm_schedule: unknown action %q (want add, list, or cancel)", action)
	}
}

func (t *scheduleTool) add(args map[string]any, options tools.RunOptions) tools.Result {
	agentType := swarmStr(args, "agent_type")
	task := swarmStr(args, "task")
	if agentType == "" {
		return errResult("swarm_schedule add requires agent_type")
	}
	if task == "" {
		return errResult("swarm_schedule add requires task")
	}

	every := swarmStr(args, "every")
	dailyAt := swarmStr(args, "daily_at")
	if every == "" && dailyAt == "" {
		return errResult("swarm_schedule add requires every or daily_at")
	}
	if every != "" && dailyAt != "" {
		return errResult("swarm_schedule add: every and daily_at are mutually exclusive")
	}

	var sch Schedule
	switch {
	case dailyAt != "":
		hour, minute, err := parseClock(dailyAt)
		if err != nil {
			return errResult("%v", err)
		}
		// Daily mode: Every=24h satisfies validation/display, but the scheduler
		// recomputes each fire from Hour:Minute so the wall-clock time holds across
		// DST instead of drifting by a fixed 24h.
		sch.Every = 24 * time.Hour
		sch.Daily = true
		sch.Hour = hour
		sch.Minute = minute
		sch.FirstDelay = nextDailyDelay(t.clock(), hour, minute)
	default:
		d, err := time.ParseDuration(every)
		if err != nil {
			return errResult("swarm_schedule add: invalid every %q: %v", every, err)
		}
		sch.Every = d
		if fd := swarmStr(args, "first_delay"); fd != "" {
			delay, err := time.ParseDuration(fd)
			if err != nil {
				return errResult("swarm_schedule add: invalid first_delay %q: %v", fd, err)
			}
			if delay < 0 {
				return errResult("swarm_schedule add: first_delay must be >= 0")
			}
			sch.FirstDelay = delay
		}
	}
	if mr, ok := swarmInt(args, "max_runs"); ok {
		sch.MaxRuns = mr
	}

	team := swarmStr(args, "team")
	id, err := t.sw.Scheduler().Add(policyFrom(options), team, agentType, task, options.Cwd, sch)
	if err != nil {
		if errors.Is(err, ErrUnknownAgentType) {
			return errResult("%v; available agent types: %s", err, strings.Join(t.sw.Registry().AgentTypes(), ", "))
		}
		return errResult("%v", err)
	}
	cadence := "every " + sch.Every.String()
	if dailyAt != "" {
		cadence = "daily at " + dailyAt
	}
	out := fmt.Sprintf("Scheduled %s as %s on team %s (%s).", agentType, id, displayTeam(team), cadence)
	res := okResult(out, "swarm", out)
	res.Meta = map[string]string{"job_id": id, "team": sanitizeName(team), "agent_type": agentType}
	return res
}

func (t *scheduleTool) list() tools.Result {
	jobs := t.sw.Scheduler().List()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	var b strings.Builder
	fmt.Fprintf(&b, "Scheduled jobs: %d\n", len(jobs))
	for _, j := range jobs {
		maxRuns := "unbounded"
		if j.MaxRuns > 0 {
			maxRuns = strconv.Itoa(j.MaxRuns)
		}
		fmt.Fprintf(&b, "  - %s [%s/%s] every %s, runs %d (max %s), skipped %d: %s\n",
			j.ID, j.AgentType, j.Team, j.Every, j.Runs, maxRuns, j.Skipped, collapse(j.Task))
	}
	out := strings.TrimRight(b.String(), "\n")
	return okResult(out, "swarm", fmt.Sprintf("%d scheduled job(s)", len(jobs)))
}

func (t *scheduleTool) cancel(args map[string]any) tools.Result {
	id := swarmStr(args, "job_id")
	if id == "" {
		return errResult("swarm_schedule cancel requires job_id")
	}
	if !t.sw.Scheduler().Cancel(id) {
		return errResult("swarm_schedule: no such job %q", id)
	}
	out := fmt.Sprintf("Cancelled scheduled job %s.", id)
	return okResult(out, "swarm", out)
}

func (t *scheduleTool) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// parseClock parses a 24-hour "HH:MM" local time of day.
func parseClock(s string) (hour, minute int, err error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("swarm_schedule: invalid daily_at %q (want HH:MM)", s)
	}
	hour, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("swarm_schedule: invalid hour in daily_at %q (want 00-23)", s)
	}
	minute, err = strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("swarm_schedule: invalid minute in daily_at %q (want 00-59)", s)
	}
	return hour, minute, nil
}

// swarmInt reads an integer argument, accepting JSON numbers or numeric strings.
func swarmInt(args map[string]any, key string) (int, bool) {
	if args == nil {
		return 0, false
	}
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		// Reject non-integer / non-finite JSON numbers so e.g. max_runs=1.9 is an
		// error rather than silently truncating to 1.
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}
