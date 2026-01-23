// Package core provides a tiny planner to produce an initial set of subtasks
// for an iteration. This is intentionally simple: it provides a predictable
// plan structure that the agent and UI can rely on.
package core

import (
	"strings"
)

// SimplePlanner generates a short list of subtasks for the given task prompt.
// The planner uses simple heuristics and is meant as a starting point; the
// LLM may replace or refine the plan when it responds.
func SimplePlanner(task string) []string {
	task = strings.TrimSpace(task)
	if task == "" {
		return []string{"1) Inspect repository structure", "2) Run tests to discover failures", "3) Propose small change", "4) Implement change and run tests", "5) Summarize and finish"}
	}

	lower := strings.ToLower(task)

	// Heuristic: if task mentions implement/add/fix/refactor/test, produce a dev workflow
	if strings.Contains(lower, "implement") || strings.Contains(lower, "add") || strings.Contains(lower, "fix") || strings.Contains(lower, "refactor") || strings.Contains(lower, "test") {
		return []string{
			"1) Analyze relevant files and tests (use list_files/read_file)",
			"2) Propose minimal change(s) with file paths and diffs",
			"3) Implement the change",
			"4) Run tests and iterate until green",
			"5) Commit summary and report completion",
		}
	}

	// Fallback generic plan
	return []string{"1) Understand the task and list relevant files", "2) Propose a small concrete plan", "3) Implement and test the change", "4) Summarize results and finish"}
}
