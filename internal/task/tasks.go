package task

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TaskStatus values
const (
	StatusPending    = "pending"
	StatusInProgress = "in-progress"
	StatusDone       = "done"
)

type Task struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Subtasks    []Task    `json:"subtasks,omitempty"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	DoneAt      time.Time `json:"done_at,omitempty"`
}

type TaskList struct {
	Prompt string `json:"prompt"`
	Tasks  []Task `json:"tasks"`
}

var idCounter int

func nextID() string {
	idCounter++
	return fmt.Sprintf("t-%d-%d", time.Now().Unix(), idCounter)
}

var sentenceSep = regexp.MustCompile(`[.!?]\s+`)

// GenerateTasks produces a high-level list of tasks from a prompt.
// It heuristically splits the prompt into candidate tasks and creates subtasks.
func GenerateTasks(prompt string) *TaskList {
	tl := &TaskList{Prompt: prompt, Tasks: []Task{}}

	s := strings.TrimSpace(prompt)
	if s == "" {
		return tl
	}

	parts := sentenceSep.Split(s, -1)
	if len(parts) == 0 {
		parts = []string{s}
	}

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		t := Task{
			ID:          nextID(),
			Title:       summarizeTitle(p),
			Description: p,
			Status:      StatusPending,
			CreatedAt:   time.Now(),
		}

		t.Subtasks = expandSubtasksFor(t.Title)

		tl.Tasks = append(tl.Tasks, t)
	}

	return tl
}

func summarizeTitle(s string) string {
	s = strings.TrimSpace(s)
	// Prefer first clause up to 6 words
	words := strings.Fields(s)
	if len(words) <= 6 {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:6], " ") + "..."
}

func expandSubtasksFor(title string) []Task {
	// Generic subtasks for development tasks
	steps := []string{"Design", "Implement", "Write tests", "Run tests", "Document", "Submit PR"}
	subs := make([]Task, 0, len(steps))
	for _, s := range steps {
		subs = append(subs, Task{ID: nextID(), Title: s, Status: StatusPending, CreatedAt: time.Now()})
	}
	return subs
}

// Save writes the task list to a JSON file at path.
func (tl *TaskList) Save(path string) error {
	if !filepath.IsAbs(path) {
		cwd, _ := os.Getwd()
		path = filepath.Join(cwd, path)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(tl)
}

// Load reads a TaskList from a JSON file.
func Load(path string) (*TaskList, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tl TaskList
	dec := json.NewDecoder(f)
	if err := dec.Decode(&tl); err != nil {
		return nil, err
	}
	return &tl, nil
}

// MarkDone marks task (and optionally its subtasks) as done, updating timestamps.
func (tl *TaskList) MarkDone(taskID string) error {
	for i := range tl.Tasks {
		if tl.Tasks[i].ID == taskID {
			markTaskDone(&tl.Tasks[i])
			return nil
		}
		// search subtasks
		for j := range tl.Tasks[i].Subtasks {
			if tl.Tasks[i].Subtasks[j].ID == taskID {
				markTaskDone(&tl.Tasks[i].Subtasks[j])
				return nil
			}
		}
	}
	return fmt.Errorf("task id not found: %s", taskID)
}

func markTaskDone(t *Task) {
	t.Status = StatusDone
	t.DoneAt = time.Now()
	for i := range t.Subtasks {
		markTaskDone(&t.Subtasks[i])
	}
}
