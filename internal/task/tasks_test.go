package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGenerateSaveLoadMarkDone(t *testing.T) {
	prompt := "Implement a feature that validates user input and adds unit tests."
	tl := GenerateTasks(prompt)
	if len(tl.Tasks) == 0 {
		t.Fatalf("expected tasks generated")
	}

	dir := t.TempDir()
	p := filepath.Join(dir, "tasks.json")
	err := tl.Save(p)
	assert.NoError(t, err)

	// Load
	loaded, err := Load(p)
	assert.NoError(t, err)
	assert.Equal(t, len(tl.Tasks), len(loaded.Tasks))

	// Mark first task done
	tid := loaded.Tasks[0].ID
	err = loaded.MarkDone(tid)
	assert.NoError(t, err)
	// Save again
	err = loaded.Save(p)
	assert.NoError(t, err)

	// Reload and assert status
	reloaded, err := Load(p)
	assert.NoError(t, err)
	found := false
	for _, tsk := range reloaded.Tasks {
		if tsk.ID == tid {
			found = true
			assert.Equal(t, StatusDone, tsk.Status)
			assert.True(t, !tsk.DoneAt.IsZero())
		}
	}
	assert.True(t, found)

	// Ensure subtasks also marked done via markTaskDone
	if len(reloaded.Tasks[0].Subtasks) > 0 {
		for _, st := range reloaded.Tasks[0].Subtasks {
			assert.Equal(t, StatusDone, st.Status)
			assert.True(t, !st.DoneAt.IsZero())
		}
	}

	// timestamp sanity
	assert.WithinDuration(t, time.Now(), reloaded.Tasks[0].DoneAt, 5*time.Second)
	// cleanup
	_ = os.Remove(p)
}
