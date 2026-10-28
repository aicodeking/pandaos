package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	exitCancelled      = 130
	busyHeartbeatEvery = 5 * time.Second
)

var (
	errCancelled = errors.New("cancelled by relay")
	errShutdown  = errors.New("panda-agentd shutting down")
)

// serve heartbeats and pulls jobs until ctx ends or the relay returns 401.
// When ctx ends it stops polling and drains running jobs (see drain).
func (d *daemon) serve(parent context.Context, c *client) error {
	// Three lifetimes: polling stops at once on shutdown; jobs and result
	// posting outlive it for the grace periods. A 401 ends all three.
	pollCtx, stopPoll := context.WithCancelCause(parent)
	jobCtx, stopJobs := context.WithCancelCause(context.WithoutCancel(parent))
	postCtx, stopPost := context.WithCancelCause(context.WithoutCancel(parent))
	defer stopPost(nil)
	fail := func(err error) {
		if errors.Is(err, errUnauthorized) {
			stopPoll(err)
			stopJobs(err)
			stopPost(err)
		}
	}
	var hb sync.WaitGroup
	hb.Add(1)
	go func() { defer hb.Done(); d.heartbeat(jobCtx, c, fail) }()

	var jobs sync.WaitGroup
	for pollCtx.Err() == nil {
		select {
		case d.sem <- struct{}{}: // wait for a free slot before asking for work
		case <-pollCtx.Done():
			continue
		}
		var job *Job
		err := retry(pollCtx, "next job", func() (err error) {
			job, err = c.nextJob(pollCtx)
			return err
		})
		if err != nil || job == nil {
			<-d.sem
			fail(err)
			continue
		}
		jobs.Add(1)
		go func(j Job) {
			defer jobs.Done()
			defer func() { <-d.sem }()
			d.handle(jobCtx, postCtx, c, j, fail)
		}(*job)
	}
	d.drain(&jobs, parent.Err() != nil, stopJobs, stopPost)
	stopJobs(nil)
	hb.Wait()
	return context.Cause(pollCtx)
}

// drain waits for running jobs. On shutdown it gives them shutdownGrace, then
// cancels them and gives them postGrace to report as cancelled.
func (d *daemon) drain(jobs *sync.WaitGroup, shutdown bool, stopJobs, stopPost context.CancelCauseFunc) {
	done := make(chan struct{})
	go func() { jobs.Wait(); close(done) }()
	if !shutdown { // 401: jobs and posting were already cancelled
		<-done
		return
	}
	if n := d.status().Running; n > 0 {
		slog.Info("shutting down; waiting for running jobs", "running", n, "grace", d.shutdownGrace.String())
	}
	if waitFor(done, d.shutdownGrace) {
		return
	}
	slog.Warn("grace period over; cancelling running jobs", "running", d.status().Running)
	stopJobs(errShutdown)
	if !waitFor(done, d.postGrace) {
		slog.Warn("giving up on reporting cancelled jobs")
		stopPost(errShutdown)
		<-done
	}
}

func waitFor(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// heartbeat runs every heartbeatEvery, or every busyHeartbeatEvery while jobs
// run so relay cancels land quickly.
func (d *daemon) heartbeat(ctx context.Context, c *client, fail func(error)) {
	for {
		ids, err := c.heartbeat(ctx, d.caps())
		if err != nil && ctx.Err() == nil {
			slog.Warn("heartbeat failed", "err", err)
			fail(err)
		}
		d.cancel(ids)
		every := d.heartbeatEvery
		if d.status().Running > 0 {
			every = min(every, busyHeartbeatEvery)
		}
		if !sleep(ctx, every) {
			return
		}
	}
}

// cancel stops the named jobs if they are running here; unknown ids are ignored.
func (d *daemon) cancel(ids []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		if stop, ok := d.running[id]; ok {
			slog.Info("cancelling job", "job", id)
			stop(errCancelled)
		}
	}
}

func (d *daemon) handle(jobCtx, postCtx context.Context, c *client, j Job, fail func(error)) {
	log := slog.With("job", j.ID)
	post := func(res Result) {
		fail(retry(postCtx, "post result "+j.ID, func() error { return c.postResult(postCtx, j.ID, res) }))
	}
	if err := validateJob(j); err != nil {
		log.Warn("rejecting job", "err", err)
		res := Result{ExitCode: -1, Stderr: "panda-agentd: " + err.Error()}
		post(res)
		d.setStatus(func(s *Status) { s.Failed++ })
		return
	}
	ctx, stop := context.WithCancelCause(jobCtx)
	defer stop(nil)
	if !d.startJob(j.ID, stop) {
		log.Warn("job already running; ignoring duplicate")
		return
	}
	res, dir := d.execute(ctx, j, log)
	post(res)
	if dir != "" && !j.KeepWorkdir {
		if err := os.RemoveAll(dir); err != nil {
			log.Warn("remove workdir failed", "dir", dir, "err", err)
		}
	}
	d.endJob(j.ID, res)
}

// execute runs a validated job in a fresh workdir and returns its result and
// the workdir ("" if none was made).
func (d *daemon) execute(ctx context.Context, j Job, log *slog.Logger) (Result, string) {
	dir, err := d.prepareWorkdir(j.ID)
	if err != nil {
		log.Error("prepare workdir failed", "err", err)
		return Result{ExitCode: -1, Stderr: "panda-agentd: workdir: " + err.Error()}, ""
	}
	log.Info("job start", "cmd", j.Argv[0], "argc", len(j.Argv), "dir", dir)
	start := time.Now()
	r := d.run(ctx, j, dir)
	res := Result{ExitCode: r.ExitCode, Stdout: truncate(r.Stdout), Stderr: truncate(r.Stderr)}
	if cause := context.Cause(ctx); errors.Is(cause, errCancelled) || errors.Is(cause, errShutdown) {
		res = Result{ExitCode: exitCancelled, Stdout: res.Stdout, Stderr: truncate(res.Stderr + "\npanda-agentd: job cancelled: " + cause.Error() + "\n"), Cancelled: true}
	}
	log.Info("job done", "exit", res.ExitCode, "cancelled", res.Cancelled, "took", time.Since(start).Round(time.Millisecond).String())
	return res, dir
}

// prepareWorkdir makes an empty jobs/<id> owned by the job user. Any stale
// dir with the same id (crash, keepWorkdir) is removed first.
func (d *daemon) prepareWorkdir(id string) (string, error) {
	dir := filepath.Join(d.jobsDir(), id)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", err
	}
	if d.owner != nil {
		if err := os.Lchown(dir, d.owner.uid, d.owner.gid); err != nil {
			os.Remove(dir)
			return "", err
		}
	}
	return dir, nil
}

func (d *daemon) startJob(id string, stop context.CancelCauseFunc) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, dup := d.running[id]; dup {
		return false
	}
	d.running[id] = stop
	d.setStatusLocked(func(s *Status) { s.Running = len(d.running) })
	return true
}

func (d *daemon) endJob(id string, res Result) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, id)
	d.setStatusLocked(func(s *Status) {
		s.Running = len(d.running)
		if res.ExitCode == 0 && !res.Cancelled {
			s.Completed++
		} else {
			s.Failed++
		}
	})
}
