package core

import "fmt"

// Tools are the tools the TUI serves to every agent, whatever its backend: name,
// description and inputSchema, in MCP's tool shape.
var Tools = []map[string]any{
	{"name": "spawn_task", "description": "Suggest an out-of-scope follow-up (dead code, stale docs, missing tests, a confirmed TODO, a security issue noticed in passing) as a separate background task. The user sees a chip and can start it as its own session in a fresh worktree. Your turn continues. The prompt must stand alone: include file paths and context. Returns a task_id for dismiss_task.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"title", "prompt", "tldr"}, "properties": map[string]any{
			"title":  map[string]any{"type": "string", "description": "Under 60 chars, imperative, e.g. \"Fix stale README badge\"."},
			"prompt": map[string]any{"type": "string", "description": "Self-contained first message for the spawned session."},
			"tldr":   map[string]any{"type": "string", "description": "One or two plain sentences: why now, and what the session will do."},
			"cwd":    map[string]any{"type": "string", "description": "Optional absolute path to a different project root."},
		}}},
	{"name": "dismiss_task", "description": "Withdraw a task chip created with spawn_task that is now stale or superseded.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"task_id"}, "properties": map[string]any{
			"task_id": map[string]any{"type": "string"},
			"reason":  map[string]any{"type": "string"},
		}}},
}

// CallTool turns a call to one of Tools into the event core answers through reply.
// An unknown tool is answered here, and yields no event.
func CallTool(name string, args map[string]any, reply func(text string, isErr bool)) Event {
	str := func(k string) string { s, _ := args[k].(string); return s }
	switch name {
	case "spawn_task":
		return TaskSuggested{Chip: Chip{Title: str("title"), Prompt: str("prompt"), Tldr: str("tldr"), Cwd: str("cwd")}, Reply: reply}
	case "dismiss_task":
		return TaskDismissed{TaskID: str("task_id"), Reply: reply}
	}
	reply(fmt.Sprintf("unknown tool %v", name), true)
	return nil
}
