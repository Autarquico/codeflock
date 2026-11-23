package job

// transitions lists every change of state later phases may make. Reconcile's
// outcomes are all in it; anything else is refused.
var transitions = map[State][]State{
	Running:   {Waiting, Stopping, Done, Failed, Lost},
	Waiting:   {Running, Stopping, Done, Failed, Lost},
	Stopping:  {Stopping, Stopped, Done, Failed}, // Done and Failed: the agent ended by itself as a Stop began
	Stopped:   {Running, Moving, Finishing},
	Done:      {},
	Failed:    {Running, Stopping},
	Lost:      {Running, Waiting, Stopping, Done, Failed},
	Moving:    {Moving, Stopped, Moved, Running}, // Running is `resume --force`
	Finishing: {Finishing, Stopped, Done},
	Moved:     {},
}

// CanTransition reports whether a Job may go from one state to another.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}
