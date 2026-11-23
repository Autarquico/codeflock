package runner

import (
	"os"

	"github.com/Autarquico/codeflock/internal/worker/execwrap"
)

// agentSessionEnv are the variables that mark the process as living inside
// another agent session. A harness that inherits them believes it is a child:
// Claude Code then does not save its transcript, which session export needs.
// Auth and user configuration (API keys, tokens, CLAUDE_CONFIG_DIR, CODEX_HOME,
// PATH, HOME, proxies) are deliberately not here.
var agentSessionEnv = []string{
	"CLAUDECODE",                    // Claude Code: "I run inside Claude Code" flag
	"CLAUDE_CODE_CHILD_SESSION",     // makes the child skip transcript saving
	"CLAUDE_CODE_ENTRYPOINT",        // how the parent was started (cli, sdk, ...)
	"CLAUDE_CODE_SESSION_ID",        // id of the parent session
	"CLAUDE_CODE_HOST_SESSION_ID",   // id of the session of the host that embeds the parent
	"CLAUDE_CODE_SESSION_KIND",      // kind of the parent session
	"CLAUDE_CODE_SESSION_NAME",      // name of the parent session
	"CLAUDE_CODE_SESSION_ATTENDED",  // whether a human attends the parent
	"CLAUDE_CODE_SESSION_LOG",       // log of the parent session
	"CLAUDE_CODE_SESSION_ORIGIN",    // origin of the parent session
	"CLAUDE_CODE_SSE_PORT",          // IDE bridge of the parent
	"CLAUDE_CODE_MESSAGING_SOCKET",  // message channel to the parent
	"CLAUDE_CODE_MESSAGING_TOKEN",   // credential of that channel
	"CLAUDE_CODE_BRIDGE_SESSION_ID", // remote bridge session of the parent
	"CLAUDE_CODE_TMUX_SESSION",      // tmux session of the parent
	"CLAUDE_CODE_RESUME_FROM_SESSION",
	"CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS", // team mode of the parent
	"CLAUDE_CODE_EXECPATH",                 // binary path of the parent run
	"CLAUDE_PID",                           // pid of the parent Claude process
	"AI_AGENT",                             // generic "run by an agent" marker
	"CODEX_SESSION_ID",                     // Codex: id of the parent session
	"CODEX_THREAD_ID",                      // Codex: thread of the parent
	"CODEX_TOOL_CALL_ID",                   // Codex: tool call that spawned us
	"CODEX_SANDBOX",                        // Codex: parent runs sandboxed
	"CODEX_NON_INTERACTIVE",                // Codex: parent is non-interactive
	"CODEX_COMPANION_SESSION_ID",           // companion plugin session of the parent
}

// ScrubAgentSession removes agentSessionEnv from the environment of this process.
func ScrubAgentSession() {
	for _, name := range agentSessionEnv {
		_ = os.Unsetenv(name)
	}
}

// RunWrapper is the body of `cflk internal wrapper`: it cleans the environment,
// right before the harness inherits it, whoever launched the Job (and whatever
// the environment of the tmux server), and runs execwrap on the launch directory.
func RunWrapper(dir string) error {
	ScrubAgentSession()
	return execwrap.Run(execwrap.Options{RunDir: dir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}
