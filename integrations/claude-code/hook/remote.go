package hook

import (
	"encoding/json"
	"io"

	"github.com/mirastacklabs-ai/mirafleet/internal/opconf"
)

// localTools maps Claude Code's built-in local tools to the equivalent remote mirafleet tool.
// In a project whose .mirafleet.yaml says `remote: true` the hook denies these.
var localTools = map[string]string{
	"Bash": "bash", "Read": "read", "Write": "write", "Edit": "edit", "MultiEdit": "multi_edit",
	"Grep": "grep", "Glob": "glob", "LS": "ls", "NotebookEdit": "notebook_edit",
}

// denyRemote handles a call to a local tool. It returns true when it wrote a deny, which happens
// when the project is remote or when the project file cannot be read or parsed (fail closed). It
// returns false (fail open, the caller carries on as before) when the tool is not a local one,
// there is no project file or remote is false.
func denyRemote(stdout io.Writer, env Env, in *input) bool {
	remote, isLocal := localTools[in.ToolName]
	if !isLocal {
		return false
	}
	dir := projectOf(env, in)
	if dir == "" {
		return false
	}
	p, path, err := opconf.LoadProject(dir, env.Home)
	if err != nil {
		write(stdout, "deny", clipRunes(oneLine("mirafleet: cannot read the project file ("+err.Error()+
			"), so local "+in.ToolName+" is blocked in case this project is remote. Fix or remove .mirafleet.yaml."), 400))
		return true
	}
	if path == "" || !p.Remote {
		return false
	}
	write(stdout, "deny", "This project is remote (.mirafleet.yaml has remote: true), so local "+in.ToolName+
		" is disabled. Use the mirafleet MCP tool `"+remote+"` ("+toolPrefix+remote+") instead; "+
		"it runs in the remote workspace. Open one first with workspace_open if you have not.")
	return true
}

func write(stdout io.Writer, decision, reason string) {
	var o hookOutput
	o.HookSpecificOutput.HookEventName = "PreToolUse"
	o.HookSpecificOutput.PermissionDecision = decision
	o.HookSpecificOutput.PermissionDecisionReason = reason
	b, _ := json.Marshal(o)
	stdout.Write(append(b, '\n'))
}
