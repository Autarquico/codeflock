package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/peer/job"
	"github.com/Autarquico/codeflock/internal/peer/server"
	"github.com/Autarquico/codeflock/internal/peer/sessions"
	"github.com/Autarquico/codeflock/internal/peer/wire"
	"github.com/Autarquico/codeflock/internal/support/atomicfile"
)

// This file is the destination of a movement. It never starts anything until it
// is told to activate, never forgets a transfer it was told about, and answers a
// repeated order with what it did the first time. Every order takes the start
// lock of the Job id first (the Job may not have a record here yet) and the
// Job's own lock whenever it reads or writes the record.

// The points of the destination of a movement.
const (
	pointWorktree  = "recv:worktree" // the worktree of the new generation exists
	pointStaged    = "recv:staged"   // the session archive is waiting
	pointPrepared2 = "recv:prepared" // `prepared` written
	pointImported  = "recv:imported" // the session is installed under its new id
	pointWritten   = "recv:written"  // the Job is written: it is this peer's
	pointLaunched  = "recv:launched" // the agent started
)

const (
	preparedFile = "prepared.json"
	abortedFile  = "aborted"
	sessionFile  = "session.tgz"
)

// prepared is what the destination records, last, when it has received a movement.
type prepared struct {
	Transfer  string              `json:"transfer"`
	Gen       int                 `json:"gen"`
	Commit    string              `json:"commit"`
	Repo      string              `json:"repo"`
	Harness   string              `json:"harness"` // the name on the wire
	CreatedBy string              `json:"created_by"`
	History   []wire.HistoryEntry `json:"history,omitempty"`
}

func readPrepared(dir string) (prepared, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, preparedFile)) //nolint:gosec // a record of this peer
	if errors.Is(err, os.ErrNotExist) {
		return prepared{}, false, nil
	}
	var rec prepared
	if err != nil || json.Unmarshal(raw, &rec) != nil {
		return prepared{}, false, fmt.Errorf("the record of the transfer in %s cannot be read", dir)
	}
	return rec, true, nil
}

func isAborted(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, abortedFile))
	return err == nil
}

func markAborted(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, abortedFile), nil, 0o600)
}

func transferAborted(id, why string) error {
	return domain.NewError(wire.CodeTransferAborted, fmt.Sprintf("the transfer of job %s was given up: %s", id, why), nil)
}

// arrivedBy says whether the record is the Job as this transfer left it here:
// at its generation, or a later one after having owned it.
func (p *peer) arrivedBy(j job.Job, transfer string, gen int) bool {
	return (j.Gen == gen && j.ArrivedBy == transfer) || (j.Gen > gen && j.OwnedGen(p.name, gen))
}

func (l *localJobs) prepare(ctx context.Context, in server.PrepareInput, archive []byte) (wire.Transfer, error) {
	out := wire.Transfer{ID: in.Transfer, Gen: in.Gen, State: wire.TransferPrepared}
	p, err := l.open()
	if err != nil {
		return out, err
	}
	harnessID, ok := harnessIDs[in.Harness]
	if !ok || in.Harness == "" {
		return out, invalid("unknown harness %q: use claude or codex", in.Harness)
	}
	manifest, err := sessions.Manifest(archive)
	if err != nil {
		return out, invalid("%v", err)
	}
	if manifest.Adapter != harnessID {
		return out, invalid("the session is of %s and the Job of %s", manifest.Adapter, harnessID)
	}
	release, err := p.store.LockStart(in.Job)
	if err != nil {
		return out, err
	}
	defer release()
	dir := p.store.TransferDir(in.Job, in.Transfer)
	if isAborted(dir) {
		return out, transferAborted(in.Job, "it was given up before it was prepared")
	}
	switch j, err := p.store.Get(in.Job); {
	case errors.Is(err, job.ErrNotFound):
	case err != nil:
		return out, err
	case j.Repo != in.Repo || j.Harness != harnessID:
		return out, domain.NewError(domain.CodeIdempotencyMismatch, fmt.Sprintf("another job with the id %s lives here", j.ID), nil)
	case p.arrivedBy(j, in.Transfer, in.Gen):
		out.State = wire.TransferActivated
		return out, nil
	case j.Gen >= in.Gen:
		return out, domain.NewError(domain.CodeStaleLease, fmt.Sprintf("job %s is at generation %d here: a movement to %d is late", j.ID, j.Gen, in.Gen), nil)
	case j.State != job.Moved:
		return out, invalid("this peer holds a live copy of job %s (%s): `cflk clean` or stop it before it can come back", j.ID, j.State)
	}
	mirror := p.mirrorDir(in.Repo)
	ref, err := wire.MirrorRef(in.Job, in.Gen)
	if err != nil {
		return out, err
	}
	if oid, err := git(ctx, mirror, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}"); err != nil || oid != in.Commit {
		return out, invalid("%s is not at %s in the mirror of %s: push the code first", ref, in.Commit, in.Repo)
	}
	want := prepared{Transfer: in.Transfer, Gen: in.Gen, Commit: in.Commit, Repo: in.Repo, Harness: in.Harness, CreatedBy: in.CreatedBy, History: in.History}
	wt := p.store.WorktreeDir(in.Job, in.Gen)
	switch have, found, err := readPrepared(dir); {
	case err != nil:
		return out, err
	case found && !equalPrepared(have, want):
		return out, invalid("transfer %s of job %s was prepared with other data", in.Transfer, in.Job)
	case found:
		if info, err := os.Stat(wt); err == nil && info.IsDir() {
			return out, nil // prepared already: nothing changes
		}
	}
	if err := p.supersede(in.Job, in.Gen, in.Transfer); err != nil {
		return out, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { // from here on the transfer is on disk, even before its record
		return out, err
	}
	p.dropWorktree(ctx, mirror, wt)
	if err := p.detachOwn(ctx, mirror, in.Job, "cflk/"+in.Job); err != nil { // a Job coming back finds its old worktree here
		return out, err
	}
	if _, err := git(ctx, mirror, "worktree", "add", "--quiet", "--force", "-B", "cflk/"+in.Job, wt, in.Commit); err != nil {
		p.dropWorktree(ctx, mirror, wt)
		return out, err
	}
	if err := p.fault(pointWorktree); err != nil {
		return out, err
	}
	if err := atomicfile.Write(filepath.Join(dir, sessionFile), archive, 0o600); err != nil {
		return out, err
	}
	if err := p.fault(pointStaged); err != nil {
		return out, err
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return out, err
	}
	if err := atomicfile.Write(filepath.Join(dir, preparedFile), raw, 0o600); err != nil {
		return out, err
	}
	return out, p.fault(pointPrepared2)
}

func equalPrepared(a, b prepared) bool {
	return a.Transfer == b.Transfer && a.Gen == b.Gen && a.Commit == b.Commit && a.Repo == b.Repo &&
		a.Harness == b.Harness && a.CreatedBy == b.CreatedBy && slices.Equal(a.History, b.History)
}

// supersede gives up the transfers of the Job that were prepared for this
// generation and not activated: a source that moves again after `resume --force`
// makes a new transfer for the same generation, and the old one must not start.
func (p *peer) supersede(id string, gen int, keep string) error {
	entries, err := os.ReadDir(filepath.Join(p.store.Dir(id), "transfers"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		dir := p.store.TransferDir(id, e.Name())
		if e.Name() == keep || isAborted(dir) {
			continue
		}
		if rec, found, err := readPrepared(dir); err == nil && found && rec.Gen == gen {
			if err := markAborted(dir); err != nil {
				return err
			}
			_ = os.Remove(filepath.Join(dir, sessionFile))
		}
	}
	return nil
}

// dropWorktree takes back a worktree of the mirror and what git registered of it.
func (p *peer) dropWorktree(ctx context.Context, repoDir, wt string) {
	bg := context.WithoutCancel(ctx)
	_, _ = git(bg, repoDir, "worktree", "remove", "--force", wt)
	_ = os.RemoveAll(wt)
	_, _ = git(bg, repoDir, "worktree", "prune")
}

// refuse gives the transfer up because the destination cannot start the Job:
// the source goes back to stopped on seeing this answer.
func (p *peer) refuse(ctx context.Context, in server.TransferInput, mirror string, cause error) error {
	if err := markAborted(p.store.TransferDir(in.Job, in.Transfer)); err != nil {
		return fmt.Errorf("giving the transfer up: %w", errors.Join(err, cause))
	}
	p.dropTransfer(ctx, in, mirror)
	return transferAborted(in.Job, cleanMessage(cause))
}

// dropTransfer removes what a given-up transfer left (its session and, when no
// record of the Job at this generation or later exists, its worktree).
func (p *peer) dropTransfer(ctx context.Context, in server.TransferInput, mirror string) {
	_ = os.Remove(filepath.Join(p.store.TransferDir(in.Job, in.Transfer), sessionFile))
	if j, err := p.store.Get(in.Job); err == nil && j.Gen >= in.Gen {
		return
	}
	if p.liveTransferFor(in.Job, in.Gen, in.Transfer) {
		return // the worktree is another transfer's now: a replayed abort must not take it
	}
	p.dropWorktree(ctx, mirror, p.store.WorktreeDir(in.Job, in.Gen))
}

// liveTransferFor says whether a transfer other than except, not given up, is
// preparing or prepared for the generation: the g<gen> worktree belongs to it. One
// with no record yet (a prepare that did not get to write it) may be building it.
func (p *peer) liveTransferFor(id string, gen int, except string) bool {
	entries, _ := os.ReadDir(filepath.Join(p.store.Dir(id), "transfers"))
	for _, e := range entries {
		dir := p.store.TransferDir(id, e.Name())
		if e.Name() == except || isAborted(dir) {
			continue
		}
		if rec, found, err := readPrepared(dir); err != nil || !found || rec.Gen == gen {
			return true
		}
	}
	return false
}

func (l *localJobs) activate(ctx context.Context, in server.TransferInput) (wire.JobView, error) {
	p, err := l.open()
	if err != nil {
		return wire.JobView{}, err
	}
	release, err := p.store.LockStart(in.Job)
	if err != nil {
		return wire.JobView{}, err
	}
	defer release()
	dir := p.store.TransferDir(in.Job, in.Transfer)
	if isAborted(dir) {
		return wire.JobView{}, transferAborted(in.Job, "it was given up before it was activated")
	}
	if j, err := p.store.Get(in.Job); err == nil && p.arrivedBy(j, in.Transfer, in.Gen) {
		if j.Gen != in.Gen || j.Launch != nil || j.State != job.Stopped {
			// Activated already: what happened to the Job since is the answer.
			cur, err := p.runner.Reconcile(ctx, in.Job)
			return wire.JobView(view(cur, "")), mapErr(err)
		}
		return l.launchArrived(ctx, p, in) // written, never launched: a crash between the two
	}
	rec, found, err := readPrepared(dir)
	switch {
	case err != nil:
		return wire.JobView{}, err
	case !found || rec.Gen != in.Gen:
		return wire.JobView{}, domain.NewError(domain.CodeNotFound, fmt.Sprintf("transfer %s of job %s was not prepared here", in.Transfer, in.Job), nil)
	}
	harnessID := harnessIDs[rec.Harness]
	mirror := p.mirrorDir(rec.Repo)
	if !p.installed(rec.Harness) {
		return wire.JobView{}, p.refuse(ctx, in, mirror, fmt.Errorf("%s is not installed on %s", rec.Harness, p.name))
	}
	archive, err := os.ReadFile(filepath.Join(dir, sessionFile)) //nolint:gosec // a file of this peer
	if err != nil {
		return wire.JobView{}, p.refuse(ctx, in, mirror, fmt.Errorf("the session was not received: %w", err))
	}
	wt := p.store.WorktreeDir(in.Job, in.Gen)
	cwd, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return wire.JobView{}, p.refuse(ctx, in, mirror, fmt.Errorf("the worktree is gone: %w", err))
	}
	imported, err := p.sessions.Import(ctx, archive, cwd)
	if err != nil {
		return wire.JobView{}, p.refuse(ctx, in, mirror, fmt.Errorf("importing the conversation: %w", err))
	}
	if err := p.fault(pointImported); err != nil {
		return wire.JobView{}, err
	}
	err = p.store.WithLockNew(in.Job, func(j *job.Job, exists bool, _ func()) error {
		if exists {
			if j.State != job.Moved || j.Gen >= in.Gen {
				return domain.NewError(domain.CodeStaleLease, fmt.Sprintf("job %s changed here: it is %s at generation %d", j.ID, j.State, j.Gen), nil)
			}
		} else {
			j.Repo, j.Harness, j.CreatedBy = rec.Repo, harnessID, rec.CreatedBy
		}
		j.Peer, j.Gen, j.State, j.SessionID = p.name, in.Gen, job.Stopped, imported.SessionID
		j.Launch, j.Move, j.MovedTo, j.Finish, j.StoppingFrom, j.ArrivedBy = nil, nil, nil, nil, "", in.Transfer
		j.History = append(historyOf(rec.History), job.Entry{Gen: in.Gen, Peer: p.name, Commit: rec.Commit})
		return nil
	})
	if err != nil {
		return wire.JobView{}, mapErr(err)
	}
	if err := p.fault(pointWritten); err != nil {
		return wire.JobView{}, err
	}
	return l.launchArrived(ctx, p, in)
}

func historyOf(h []wire.HistoryEntry) []job.Entry {
	out := make([]job.Entry, len(h))
	for i, e := range h {
		out[i] = job.Entry{Gen: e.Gen, Peer: e.Peer, Commit: e.Commit}
	}
	return out
}

// launchArrived starts the agent of a Job that was written here. From the write
// on the Job is this peer's, however the launch goes: if it came to nothing the
// Job stays stopped here with the problem, `cflk resume` (or the source's retry,
// which launches again) tries once more, and the answer is that state.
func (l *localJobs) launchArrived(ctx context.Context, p *peer, in server.TransferInput) (wire.JobView, error) {
	launched, err := p.runner.Launch(ctx, in.Job, "")
	if err != nil {
		cur, getErr := p.store.Get(in.Job)
		if getErr != nil {
			return wire.JobView{}, errors.Join(err, getErr)
		}
		return wire.JobView(view(cur, clip(err.Error()))), nil
	}
	if err := p.fault(pointLaunched); err != nil {
		return wire.JobView{}, err
	}
	return wire.JobView(view(launched, "")), nil
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400]
	}
	return s
}

func (l *localJobs) abortTransfer(ctx context.Context, in server.TransferInput) (wire.Transfer, error) {
	out := wire.Transfer{ID: in.Transfer, Gen: in.Gen, State: wire.TransferAborted}
	p, err := l.open()
	if err != nil {
		return out, err
	}
	release, err := p.store.LockStart(in.Job)
	if err != nil {
		return out, err
	}
	defer release()
	if j, err := p.store.Get(in.Job); err == nil && p.arrivedBy(j, in.Transfer, in.Gen) {
		out.State = wire.TransferActivated
		return out, nil
	}
	dir := p.store.TransferDir(in.Job, in.Transfer)
	if err := markAborted(dir); err != nil {
		return out, err
	}
	if rec, found, _ := readPrepared(dir); found {
		p.dropTransfer(ctx, in, p.mirrorDir(rec.Repo))
	}
	return out, nil
}
