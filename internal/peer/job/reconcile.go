package job

// Facts is what a command observes about the current launch of a Job.
type Facts struct {
	SessionAlive bool // the tmux session exists
	Waiting      bool // the waiting marker is in the launch directory
	Exit         *int // exit code from the exit record; nil if there is none
	GroupEmpty   bool // no process of the launch's process group remains
}

// Reconcile returns the state a Job should have given what is observed. It
// only acts on running, waiting and lost; every other state is returned as is.
//
//   - session alive: running, or waiting if the marker is set;
//   - exit record: done (code 0) or failed, only with the process group empty;
//   - neither: lost, only with the process group empty.
//
// If the outcome is done, failed or lost but a process remains, the Job is
// stopping and needs `cflk stop`.
func Reconcile(state State, f Facts) State {
	switch state {
	case Running, Waiting, Lost:
	default:
		return state
	}
	switch {
	case f.SessionAlive && f.Waiting:
		return Waiting
	case f.SessionAlive:
		return Running
	case !f.GroupEmpty:
		return Stopping
	case f.Exit == nil:
		return Lost
	case *f.Exit == 0:
		return Done
	default:
		return Failed
	}
}
