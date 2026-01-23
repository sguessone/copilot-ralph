package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sguessone/copilot-ralph/internal/task"
	"github.com/stretchr/testify/require"
)

func TestPlanAndWorkFlow(t *testing.T) {
	dir := t.TempDir()
	taskFile := filepath.Join(dir, "tasks.json")
	workDir := filepath.Join(dir, "work")

	// Run plan: ralph plan --prompt "Do X" --out taskFile
	rootCmd.SetArgs([]string{"plan", "--prompt", "Implement logging and tests", "--out", taskFile})
	err := Execute()
	require.NoError(t, err)

	// Ensure tasks file exists
	_, err = os.Stat(taskFile)
	require.NoError(t, err)

	// Run work: ralph work --file taskFile --workdir workDir
	rootCmd.SetArgs([]string{"work", "--file", taskFile, "--workdir", workDir})
	err = Execute()
	require.NoError(t, err)

	// Verify work dir contains files
	infos, err := os.ReadDir(workDir)
	require.NoError(t, err)
	require.Greater(t, len(infos), 0)

	// Verify tasks file updated and tasks are marked done
	tl, err := task.Load(taskFile)
	require.NoError(t, err)
	for _, tt := range tl.Tasks {
		require.Equal(t, task.StatusDone, tt.Status)
	}
}
