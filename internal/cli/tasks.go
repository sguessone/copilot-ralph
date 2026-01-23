package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sguessone/copilot-ralph/internal/core"
	"github.com/sguessone/copilot-ralph/internal/sdk"
	"github.com/sguessone/copilot-ralph/internal/task"
)

var (
	planPrompt string
	planOut    string
	workFile   string
	workDir    string
)

func init() {
	rootCmd.AddCommand(planCmd)
	rootCmd.AddCommand(workCmd)

	planCmd.Flags().StringVar(&planPrompt, "prompt", "", "task prompt (text)")
	planCmd.Flags().StringVar(&planOut, "out", "tasks.json", "path to write tasks JSON")

	workCmd.Flags().StringVar(&workFile, "file", "tasks.json", "tasks JSON file to process")
	workCmd.Flags().StringVar(&workDir, "workdir", "work", "directory where placeholder work files will be created")
}

var planCmd = &cobra.Command{
	Use:   "plan",
	Short: "Generate task list JSON from a prompt",
	RunE: func(cmd *cobra.Command, args []string) error {
		prompt := planPrompt
		if prompt == "" && len(args) > 0 {
			prompt = strings.Join(args, " ")
		}
		if prompt == "" {
			return fmt.Errorf("prompt required (pass --prompt or args)")
		}

		tl := task.GenerateTasks(prompt)
		if err := tl.Save(planOut); err != nil {
			return err
		}

		fmt.Printf("Wrote %d tasks to %s\n", len(tl.Tasks), planOut)
		return nil
	},
}

var workCmd = &cobra.Command{
	Use:   "work",
	Short: "Process tasks JSON and mark tasks as done (creates placeholder files)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if workFile == "" {
			workFile = "tasks.json"
		}
		tl, err := task.Load(workFile)
		if err != nil {
			return err
		}

		// ensure workdir exists
		outdir := workDir
		if outdir == "" {
			outdir = "work"
		}
		if err := ensureDir(outdir); err != nil {
			return err
		}

		// Integrate with SDK: create client and session to ask AI to implement subtasks
		cfg := &core.LoopConfig{Prompt: "task runner", Model: "gpt-5-mini", WorkingDir: ".", MaxIterations: 1, Timeout: 60 * time.Second}
		client, err := createSDKClient(cfg)
		if err != nil {
			return err
		}
		defer client.Stop()

		if err := client.Start(); err != nil {
			fmt.Printf("Warning: SDK client unavailable, falling back to placeholder work: %v\n", err)
			// fallback: create placeholder files as before
			for _, t := range tl.Tasks {
				fname := sanitizeFilename(t.Title) + ".task.txt"
				path := filepath.Join(outdir, fname)
				content := fmt.Sprintf("Task: %s\nDescription: %s\nStatus: completed\n", t.Title, t.Description)
				if err := osWrite(path, content); err != nil {
					return err
				}
				if err := tl.MarkDone(t.ID); err != nil {
					return err
				}
				if err := tl.Save(workFile); err != nil {
					return err
				}
				fmt.Printf("Completed task %s -> %s\n", t.Title, path)
			}
			fmt.Println("All tasks processed (placeholder mode)")
			return nil
		}

		ctx := context.Background()
		if err := client.CreateSession(ctx); err != nil {
			fmt.Printf("Warning: SDK session unavailable, falling back to placeholder work: %v\n", err)
			for _, t := range tl.Tasks {
				fname := sanitizeFilename(t.Title) + ".task.txt"
				path := filepath.Join(outdir, fname)
				content := fmt.Sprintf("Task: %s\nDescription: %s\nStatus: completed\n", t.Title, t.Description)
				if err := osWrite(path, content); err != nil {
					return err
				}
				if err := tl.MarkDone(t.ID); err != nil {
					return err
				}
				if err := tl.Save(workFile); err != nil {
					return err
				}
				fmt.Printf("Completed task %s -> %s\n", t.Title, path)
			}
			fmt.Println("All tasks processed (placeholder mode)")
			return nil
		}

		// process each top-level task by asking the AI to implement its subtasks
		for _, t := range tl.Tasks {
			for _, st := range t.Subtasks {
				prompt := fmt.Sprintf("Implement the subtask.\nTitle: %s\nDescription: %s\nRespond ONLY with a JSON array of objects {\"path\": string, \"content\": string} for files to create/modify.", st.Title, st.Title)

				// Try multiple attempts to get valid JSON from the AI
				var files []struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				var attemptErr error
				maxAttempts := 3
				for attempt := 1; attempt <= maxAttempts; attempt++ {
					// per-attempt timeout
					attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					// cancel must be called per-iteration
					defer cancel()

					events, err := client.SendPrompt(attemptCtx, prompt)
					if err != nil {
						attemptErr = err
						break
					}

					collected := collectTextFromEvents(events)

					// try to extract JSON array from collected text
					jtxt, jerr := extractJSONArray(collected)
					if jerr != nil {
						attemptErr = jerr
						// prepare correction prompt for next attempt
						prompt = fmt.Sprintf("Previous response was not valid JSON: %v. Please respond ONLY with a valid JSON array of objects {\"path\":string,\"content\":string}.", jerr)
						continue
					}

					if err := json.Unmarshal([]byte(jtxt), &files); err != nil {
						attemptErr = err
						prompt = fmt.Sprintf("Previous JSON could not be parsed: %v. Please respond ONLY with valid JSON array.", err)
						continue
					}

					// Ask AI to validate the parsed JSON before applying
					validatePayload, _ := json.Marshal(files)
					vPrompt := fmt.Sprintf("I will apply the following file changes: %s\nIf this is correct, reply with VALID. Otherwise, reply ONLY with a corrected JSON array of the same format.", string(validatePayload))
					vEvents, verr := client.SendPrompt(attemptCtx, vPrompt)
					if verr != nil {
						// if validate call fails, accept the files as-is
						attemptErr = nil
						break
					}
					vText := collectTextFromEvents(vEvents)
					vTextTrim := strings.TrimSpace(strings.ToUpper(vText))
					if vTextTrim == "VALID" || vTextTrim == "OK" || strings.Contains(vTextTrim, "VALID") {
						// accepted
						attemptErr = nil
						break
					}

					// try to extract corrected JSON from AI validation reply
					corrected, cerr := extractJSONArray(vText)
					if cerr != nil {
						attemptErr = errors.New("validation reply not JSON or not VALID")
						prompt = fmt.Sprintf("Previous response was not valid JSON: %v. Please respond ONLY with a valid JSON array of objects {\"path\":string,\"content\":string}.", cerr)
						continue
					}

					if err := json.Unmarshal([]byte(corrected), &files); err != nil {
						attemptErr = err
						prompt = fmt.Sprintf("Corrected JSON could not be parsed: %v. Please respond ONLY with valid JSON array.", err)
						continue
					}

					attemptErr = nil
					break
				}

				if attemptErr != nil {
					fmt.Printf("Failed to obtain valid JSON for subtask %s: %v\n", st.Title, attemptErr)
					continue
				}

				for _, f := range files {
					outPath := f.Path
					if !filepath.IsAbs(outPath) {
						outPath = filepath.Join(outdir, outPath)
					}
					if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
						return err
					}
					if err := os.WriteFile(outPath, []byte(f.Content), 0644); err != nil {
						return err
					}
					fmt.Printf("Wrote %s\n", outPath)
				}

				// mark subtask done and persist
				if err := tl.MarkDone(st.ID); err != nil {
					return err
				}
				if err := tl.Save(workFile); err != nil {
					return err
				}
				fmt.Printf("Completed subtask %s\n", st.Title)
			}

			// mark top-level task done after subtasks
			if err := tl.MarkDone(t.ID); err != nil {
				return err
			}
			if err := tl.Save(workFile); err != nil {
				return err
			}
			fmt.Printf("Completed task %s\n", t.Title)
		}

		fmt.Println("All tasks processed")
		return nil
	},
}

func ensureDir(d string) error {
	p, err := filepath.Abs(d)
	if err != nil {
		return err
	}
	return osMkdirAll(p)
}

func sanitizeFilename(s string) string {
	// keep letters, numbers, dash and underscore
	re := regexp.MustCompile(`[^a-zA-Z0-9\-_]+`)
	out := re.ReplaceAllString(s, "-")
	return strings.Trim(out, "-")
}

// Adapter functions to avoid importing os/regxp at top-level of this file
// (keeps imports minimal and explicit for tests)
func osWrite(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func osMkdirAll(path string) error {
	return os.MkdirAll(path, 0755)
}

var (
	// import-time references to avoid unused import errors in small changes
	_ = filepath.Base
)

// collectTextFromEvents consumes events channel and returns concatenated text
func collectTextFromEvents(events <-chan sdk.Event) string {
	var sb strings.Builder
	for ev := range events {
		switch e := ev.(type) {
		case *sdk.TextEvent:
			sb.WriteString(e.Text)
		default:
			sb.WriteString(fmt.Sprint(e))
		}
	}
	return sb.String()
}

// extractJSONArray extracts the first JSON array (from '[' to matching ']') found in s.
func extractJSONArray(s string) (string, error) {
	i := strings.Index(s, "[")
	if i < 0 {
		return "", errors.New("no JSON array start found")
	}
	j := strings.LastIndex(s, "]")
	if j <= i {
		return "", errors.New("no JSON array end found")
	}
	candidate := s[i : j+1]
	// quick validation: ensure balanced brackets
	depth := 0
	for _, ch := range candidate {
		if ch == '[' {
			depth++
		}
		if ch == ']' {
			depth--
		}
		if depth < 0 {
			return "", errors.New("unbalanced brackets in candidate JSON")
		}
	}
	if depth != 0 {
		return "", errors.New("unbalanced brackets in candidate JSON")
	}
	return candidate, nil
}
