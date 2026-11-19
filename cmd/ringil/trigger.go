// schedule.go: scheduled jobs from the terminal, run by serve
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agilira/go-errors"
	"github.com/agilira/orpheus/pkg/orpheus"

	"github.com/agilira/ringil-agent-sdk/schedule"
	"github.com/agilira/ringil-agent-sdk/serve"
)

func (c *cli) scheduleStore() (*schedule.Store, error) {
	base, err := c.configDir()
	if err != nil {
		return nil, errors.Wrap(err, ErrUsage, "no user config directory for the schedule")
	}
	return schedule.OpenStore(filepath.Join(base, "ringil", "schedule"))
}

// cmdSchedule lists the jobs: when they run next and how the last run went.
func (c *cli) cmdSchedule(*orpheus.Context) error {
	store, err := c.scheduleStore()
	if err != nil {
		return err
	}
	defer closeQuietly(store)
	jobs, err := store.Jobs()
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return c.line("No scheduled jobs. Add one with: ringil schedule add NAME --when \"0 8 * * 1-5\" PROMPT... (or - for standard input)")
	}
	status, err := store.Status()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := c.line(describeJob(j, status[j.Name], time.Now())); err != nil {
			return err
		}
	}
	return nil
}

func describeJob(j schedule.Job, st schedule.Status, now time.Time) string {
	next := "next " + nextRun(j, now)
	if j.Paused {
		next = "paused"
	}
	profile := j.Profile
	if profile == "" {
		profile = "default"
	}
	text := clean(j.Name) + "  " + clean(j.When) + "  (" + clean(profile) + ", " + clean(next) + ")\n    " + clean(oneLine(j.Input, 100))
	switch {
	case st.LastFire.IsZero():
	case st.LastEnd.Before(st.LastFire):
		text += "\n    running since " + st.LastFire.Local().Format(time.DateTime)
	case st.LastError != "":
		text += "\n    last run " + st.LastFire.Local().Format(time.DateTime) + " failed: " + clean(oneLine(st.LastError, 200))
	default:
		text += "\n    last run " + st.LastFire.Local().Format(time.DateTime) + ": " + clean(oneLine(st.LastText, 200))
	}
	if st.Skipped > 0 {
		text += "\n    skipped " + strconv.Itoa(st.Skipped) + " time(s): the run before was still going"
	}
	return text
}

func nextRun(j schedule.Job, now time.Time) string {
	when, err := j.Check()
	if err != nil {
		return "never: " + err.Error()
	}
	next := when.Next(now)
	if next.IsZero() {
		return "never"
	}
	return next.Local().Format("Mon " + time.DateTime)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}

// cmdScheduleAdd adds or replaces a job; the prompt is the rest of the
// line, or standard input with "-".
func (c *cli) cmdScheduleAdd(ctx *orpheus.Context) error {
	args := ctx.Positional()
	if len(args) < 2 {
		return errors.New(ErrUsage, "usage: ringil schedule add NAME --when SPEC [--profile P] [--conversation C] PROMPT...")
	}
	input, err := c.jobPrompt(args[1:])
	if err != nil {
		return err
	}
	job := schedule.Job{
		Name: args[0], When: ctx.GetFlagString("when"), TimeZone: ctx.GetFlagString("time-zone"),
		Profile: ctx.GetFlagString("profile"), Conversation: ctx.GetFlagString("conversation"),
		CatchUp: ctx.GetFlagBool("catch-up"), Input: input,
	}
	if job.Profile != "" && job.Profile != "default" {
		roles, err := c.loadProfiles(ctx)
		if err != nil {
			return err
		}
		if _, ok := roles[job.Profile]; !ok {
			return errors.New(ErrUsage, "no profile "+job.Profile+" in "+c.profilesPath(ctx))
		}
	}
	store, err := c.scheduleStore()
	if err != nil {
		return err
	}
	defer closeQuietly(store)
	if err := store.Put(job); err != nil {
		return err
	}
	return c.line("Scheduled " + job.Name + ": next run " + nextRun(job, time.Now()) + ". Jobs run while `ringil serve` is running.")
}

// jobPrompt is the words given, or standard input for "-".
func (c *cli) jobPrompt(words []string) (string, error) {
	input := strings.Join(words, " ")
	if input != "-" {
		return input, nil
	}
	data, err := io.ReadAll(io.LimitReader(c.in, schedule.MaxInput+1))
	if err != nil {
		return "", errors.Wrap(err, ErrUsage, "cannot read the prompt from standard input")
	}
	return string(data), nil
}

func (c *cli) cmdScheduleRemove(ctx *orpheus.Context) error {
	return c.eachJob(ctx, "schedule rm", func(s *schedule.Store, name string) error { return s.Remove(name) })
}

func (c *cli) cmdSchedulePause(ctx *orpheus.Context) error {
	return c.eachJob(ctx, "schedule pause", func(s *schedule.Store, name string) error { return s.Pause(name, true) })
}

func (c *cli) cmdScheduleResume(ctx *orpheus.Context) error {
	return c.eachJob(ctx, "schedule resume", func(s *schedule.Store, name string) error { return s.Pause(name, false) })
}

func (c *cli) eachJob(ctx *orpheus.Context, usage string, do func(*schedule.Store, string) error) error {
	names := ctx.Positional()
	if len(names) == 0 {
		return errors.New(ErrUsage, "usage: ringil "+usage+" NAME...")
	}
	store, err := c.scheduleStore()
	if err != nil {
		return err
	}
	defer closeQuietly(store)
	for _, n := range names {
		if err := do(store, n); err != nil {
			return err
		}
	}
	return nil
}

// startSchedule runs the jobs while serve runs, each through the server
// like a run of the API: visible, with its approvals waiting there.
func (c *cli) startSchedule(ctx context.Context, srv *serve.Server) (func(), error) {
	store, err := c.scheduleStore()
	if err != nil {
		return nil, err
	}
	sched, err := schedule.New(schedule.Config{Store: store, Run: scheduledRun(srv), OnEvent: c.scheduleEvent})
	if err != nil {
		closeQuietly(store)
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sched.Run(ctx); err != nil {
			c.note("scheduled jobs are not run: " + err.Error())
		}
	}()
	return func() {
		cancel()
		<-done
		closeQuietly(store)
	}, nil
}

// scheduledRun starts a job on the server and waits for it.
func scheduledRun(srv *serve.Server) schedule.Run {
	return func(ctx context.Context, j schedule.Job) (schedule.Outcome, error) {
		profile := j.Profile
		if profile == "" {
			profile = "default"
		}
		snap, err := srv.Start(profile, j.Conversation, j.Input)
		if err != nil {
			return schedule.Outcome{}, err
		}
		end, err := srv.Wait(ctx, snap.ID)
		if err != nil {
			return schedule.Outcome{Run: snap.ID}, err
		}
		out := schedule.Outcome{Run: snap.ID, Text: end.Text}
		switch {
		case end.State == serve.Completed:
			return out, nil
		case end.Error != nil:
			return out, errors.New(ErrUsage, string(end.State)+": "+end.Error.Message)
		}
		return out, errors.New(ErrUsage, "the run ended "+string(end.State))
	}
}

func (c *cli) scheduleEvent(e schedule.Event) {
	job := clean(e.Job)
	switch e.Kind {
	case schedule.JobStarted:
		c.note("job " + job + " started")
	case schedule.JobFinished:
		c.note("job " + job + " finished (run " + e.Run + ")")
	case schedule.JobFailed:
		c.note("job " + job + " failed (run " + e.Run + "): " + clean(oneLine(e.Err.Error(), 300)))
	case schedule.JobSkipped:
		c.note("job " + job + " skipped: its last run is still going")
	case schedule.JobInvalid:
		c.note("schedule: " + clean(oneLine(e.Err.Error(), 300)))
	}
}
