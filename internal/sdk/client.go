// Package sdk provides a wrapper around the GitHub Copilot SDK.
//
// This package abstracts Copilot SDK integration, providing session management,
// event handling, custom tool registration, and error handling. It provides
// a simplified interface for Ralph's needs while handling the complexity of
// the underlying SDK.
//
// See specs/sdk-integration.md for detailed specification.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/sguessone/copilot-ralph/internal/indexer"
)

// Default configuration values.
const (
	DefaultModel     = "gpt-4"
	DefaultLogLevel  = "info"
	DefaultTimeout   = 60 * time.Second
	DefaultStreaming = true
)

// Retry backoff durations for transient errors.
var retryBackoffs = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
}

// isRetryableError determines if an error is transient and can be retried.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// HTTP/2 connection errors are transient
	if strings.Contains(errStr, "GOAWAY") {
		return true
	}
	if strings.Contains(errStr, "connection reset") {
		return true
	}
	if strings.Contains(errStr, "connection refused") {
		return true
	}
	if strings.Contains(errStr, "connection terminated") {
		return true
	}
	if strings.Contains(errStr, "EOF") {
		return true
	}
	if strings.Contains(errStr, "timeout") {
		return true
	}
	return false
}

// CopilotClient wraps the GitHub Copilot SDK.
// It provides session management, event handling, and tool registration.
type CopilotClient struct {
	sdkClient         *copilot.Client
	sdkSession        *copilot.Session
	model             string
	logLevel          string
	workingDir        string
	systemMessageMode string
	systemMessage     string
	timeout           time.Duration
	streaming         bool
	started           bool
	toolDefs          []copilot.Tool
}

// clientConfig holds configuration options for the client.
type clientConfig struct {
	model             string
	logLevel          string
	workingDir        string
	systemMessageMode string
	systemMessage     string
	timeout           time.Duration
	streaming         bool
}

// ClientOption configures the CopilotClient.
type ClientOption func(*clientConfig)

// WithModel sets the AI model to use.
func WithModel(model string) ClientOption {
	return func(c *clientConfig) {
		c.model = model
	}
}

// WithLogLevel sets the logging level (debug, info, warn, error).
func WithLogLevel(level string) ClientOption {
	return func(c *clientConfig) {
		c.logLevel = level
	}
}

// WithWorkingDir sets the working directory for file operations.
func WithWorkingDir(dir string) ClientOption {
	return func(c *clientConfig) {
		c.workingDir = dir
	}
}

// WithStreaming enables or disables streaming responses.
func WithStreaming(streaming bool) ClientOption {
	return func(c *clientConfig) {
		c.streaming = streaming
	}
}

// WithSystemMessage sets the system message for the session.
// Mode can be "append" or "replace".
func WithSystemMessage(message, mode string) ClientOption {
	return func(c *clientConfig) {
		c.systemMessage = message
		c.systemMessageMode = mode
	}
}

// WithTimeout sets the request timeout.
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *clientConfig) {
		c.timeout = timeout
	}
}

// NewCopilotClient creates a new Copilot SDK client with the given options.
// It returns an error if the configuration is invalid.
func NewCopilotClient(opts ...ClientOption) (*CopilotClient, error) {
	// Default configuration
	config := &clientConfig{
		model:             DefaultModel,
		logLevel:          DefaultLogLevel,
		workingDir:        ".",
		streaming:         DefaultStreaming,
		systemMessageMode: "append",
		timeout:           DefaultTimeout,
	}

	// Apply options
	for _, opt := range opts {
		opt(config)
	}

	// Validate configuration
	if config.model == "" {
		return nil, fmt.Errorf("model cannot be empty")
	}

	if config.timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive")
	}

	return &CopilotClient{
		model:             config.model,
		logLevel:          config.logLevel,
		workingDir:        config.workingDir,
		streaming:         config.streaming,
		systemMessageMode: config.systemMessageMode,
		systemMessage:     config.systemMessage,
		timeout:           config.timeout,
		started:           false,
	}, nil
}

// startLocked starts the client (must be called with lock held).
func (c *CopilotClient) Start() error {
	if c.started {
		return nil
	}

	// Initialize the SDK client with options
	c.sdkClient = copilot.NewClient(&copilot.ClientOptions{
		LogLevel: c.logLevel,
		Cwd:      c.workingDir,
	})

	// Start the SDK client
	if err := c.sdkClient.Start(); err != nil {
		// Detect common protocol-version mismatch errors from the SDK/server
		if strings.Contains(err.Error(), "protocol version mismatch") || strings.Contains(err.Error(), "SDK protocol version mismatch") {
			return fmt.Errorf("failed to start SDK client: %w; suggestion: update the SDK dependency (github.com/github/copilot-sdk/go) to a version compatible with your Copilot CLI (for example run: go get github.com/github/copilot-sdk/go@latest && go mod tidy), or update/downgrade the Copilot CLI to match the SDK protocol", err)
		}

		return fmt.Errorf("failed to start SDK client: %w", err)
	}

	c.started = true
	return nil
}

// Stop stops the client and releases resources.
func (c *CopilotClient) Stop() error {
	if !c.started {
		return nil
	}

	// Destroy any active SDK session
	if c.sdkSession != nil {
		_ = c.sdkSession.Destroy()
		c.sdkSession = nil
	}

	// Stop the SDK client
	if c.sdkClient != nil {
		_ = c.sdkClient.Stop()
		c.sdkClient = nil
	}

	c.started = false
	return nil
}

// CreateSession creates a new Copilot session.
// It initializes the SDK session resources and registers them with the client.
func (c *CopilotClient) CreateSession(ctx context.Context) error {
	if c.sdkClient == nil {
		return fmt.Errorf("SDK client not initialized")
	}

	// Build session config for the SDK
	sessionConfig := &copilot.SessionConfig{
		Model:     c.model,
		Streaming: c.streaming,
		Tools:     c.toolDefs,
	}

	// Configure system message if provided
	if c.systemMessage != "" {
		sessionConfig.SystemMessage = &copilot.SystemMessageConfig{
			Mode:    c.systemMessageMode,
			Content: c.systemMessage,
		}
	}

	// Create SDK session
	sdkSession, err := c.sdkClient.CreateSession(sessionConfig)
	if err != nil {
		return fmt.Errorf("failed to create SDK session: %w", err)
	}

	// Store SDK session reference; we no longer maintain a local Session wrapper
	c.sdkSession = sdkSession
	// Register runtime tool handlers with the SDK session if any
	// The copilot SDK will invoke ToolHandler functions when tools are called.
	// Note: tools are provided via SessionConfig.Tools at session creation time, so
	// additional registration is not required here.
	return nil
}

// RegisterTool registers a tool to be exposed to the Copilot CLI for sessions.
// Must be called before CreateSession.
func (c *CopilotClient) RegisterTool(tool copilot.Tool) error {
	if c.started {
		return fmt.Errorf("cannot register tool after client started")
	}
	c.toolDefs = append(c.toolDefs, tool)
	return nil
}

// RegisterDefaultTools registers common helpers used by Ralph: `read_file` and `list_files`.
// Call this before CreateSession to expose the tools to the agent.
func (c *CopilotClient) RegisterDefaultTools() error {
	// read_file: {path: string}
	readFileTool := copilot.Tool{
		Name:        "read_file",
		Description: "Read a text file from the workspace; parameters: path (string)",
		Parameters:  map[string]interface{}{"path": "string"},
		Handler: func(invocation copilot.ToolInvocation) (copilot.ToolResult, error) {
			// Arguments may be string or object; try to extract path
			var path string
			switch a := invocation.Arguments.(type) {
			case string:
				path = a
			case map[string]interface{}:
				if p, ok := a["path"].(string); ok {
					path = p
				}
			}

			if path == "" {
				return copilot.ToolResult{ResultType: "error", Error: "missing path parameter"}, fmt.Errorf("missing path parameter")
			}

			full := path
			// If relative path, use current working dir of client if set
			if !strings.HasPrefix(path, "/") && c.workingDir != "" {
				full = filepath.Join(c.workingDir, path)
			}

			data, err := os.ReadFile(full)
			if err != nil {
				return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
			}

			return copilot.ToolResult{TextResultForLLM: string(data), ResultType: "text"}, nil
		},
	}

	if err := c.RegisterTool(readFileTool); err != nil {
		return err
	}

	// list_files: {pattern: string}
	listFilesTool := copilot.Tool{
		Name:        "list_files",
		Description: "List files matching glob pattern; parameters: pattern (string)",
		Parameters:  map[string]interface{}{"pattern": "string"},
		Handler: func(invocation copilot.ToolInvocation) (copilot.ToolResult, error) {
			var pattern string
			switch a := invocation.Arguments.(type) {
			case string:
				pattern = a
			case map[string]interface{}:
				if p, ok := a["pattern"].(string); ok {
					pattern = p
				}
			}

			if pattern == "" {
				return copilot.ToolResult{ResultType: "error", Error: "missing pattern parameter"}, fmt.Errorf("missing pattern parameter")
			}

			// Resolve relative pattern against working dir
			globPattern := pattern
			if !strings.HasPrefix(pattern, "/") && c.workingDir != "" {
				globPattern = filepath.Join(c.workingDir, pattern)
			}

			matches, err := filepath.Glob(globPattern)
			if err != nil {
				return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
			}

			// Join results
			res := strings.Join(matches, "\n")
			return copilot.ToolResult{TextResultForLLM: res, ResultType: "text"}, nil
		},
	}

	// Register list_files
	if err := c.RegisterTool(listFilesTool); err != nil {
		return err
	}

	// run_tests: optional parameters: cmd (string), timeout (seconds)
	runTestsTool := copilot.Tool{
		Name:        "run_tests",
		Description: "Run test command in the repository; parameters: cmd (string, optional), timeout (seconds, optional)",
		Parameters:  map[string]interface{}{"cmd": "string", "timeout": "number"},
		Handler: func(invocation copilot.ToolInvocation) (copilot.ToolResult, error) {
			cmdStr := "go test ./..."
			timeoutSec := 120

			switch a := invocation.Arguments.(type) {
			case string:
				if a != "" {
					cmdStr = a
				}
			case map[string]interface{}:
				if cval, ok := a["cmd"].(string); ok && cval != "" {
					cmdStr = cval
				}
				if tval, ok := a["timeout"].(float64); ok && tval > 0 {
					timeoutSec = int(tval)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
			defer cancel()

			execCmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdStr)
			if c.workingDir != "" {
				execCmd.Dir = c.workingDir
			}

			var out bytes.Buffer
			execCmd.Stdout = &out
			execCmd.Stderr = &out

			err := execCmd.Run()
			res := out.String()
			if err != nil {
				return copilot.ToolResult{TextResultForLLM: res, ResultType: "error", Error: err.Error()}, err
			}

			return copilot.ToolResult{TextResultForLLM: res, ResultType: "success"}, nil
		},
	}

	if err := c.RegisterTool(runTestsTool); err != nil {
		return err
	}

	// run_build: optional parameters: cmd (string), timeout (seconds)
	runBuildTool := copilot.Tool{
		Name:        "run_build",
		Description: "Run build command in the repository; parameters: cmd (string, optional), timeout (seconds, optional)",
		Parameters:  map[string]interface{}{"cmd": "string", "timeout": "number"},
		Handler: func(invocation copilot.ToolInvocation) (copilot.ToolResult, error) {
			cmdStr := "go build ./..."
			timeoutSec := 120

			switch a := invocation.Arguments.(type) {
			case string:
				if a != "" {
					cmdStr = a
				}
			case map[string]interface{}:
				if cval, ok := a["cmd"].(string); ok && cval != "" {
					cmdStr = cval
				}
				if tval, ok := a["timeout"].(float64); ok && tval > 0 {
					timeoutSec = int(tval)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
			defer cancel()

			execCmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdStr)
			if c.workingDir != "" {
				execCmd.Dir = c.workingDir
			}

			var out bytes.Buffer
			execCmd.Stdout = &out
			execCmd.Stderr = &out

			err := execCmd.Run()
			res := out.String()
			if err != nil {
				return copilot.ToolResult{TextResultForLLM: res, ResultType: "error", Error: err.Error()}, err
			}

			return copilot.ToolResult{TextResultForLLM: res, ResultType: "success"}, nil
		},
	}

	if err := c.RegisterTool(runBuildTool); err != nil {
		return err
	}

	// search_index: op=index|query, root (optional), save (optional), q/query (string), k (number)
	searchTool := copilot.Tool{
		Name:        "search_index",
		Description: "Index repository and query snippets; parameters: op (index|query), root (optional), save (optional), q/query (string), k (number)",
		Parameters:  map[string]interface{}{"op": "string", "root": "string", "save": "string", "q": "string", "k": "number", "rebuild": "boolean"},
		Handler: func(invocation copilot.ToolInvocation) (copilot.ToolResult, error) {
			var args map[string]interface{}
			var asStr string
			switch a := invocation.Arguments.(type) {
			case string:
				asStr = a
			case map[string]interface{}:
				args = a
			}

			op := "query"
			if args != nil {
				if v, ok := args["op"].(string); ok && v != "" {
					op = v
				}
			}

			// default index path
			idxPath := filepath.Join(c.workingDir, ".repo_index.json")
			if args != nil {
				if v, ok := args["save"].(string); ok && v != "" {
					if strings.HasPrefix(v, "/") {
						idxPath = v
					} else {
						idxPath = filepath.Join(c.workingDir, v)
					}
				}
				if v, ok := args["root"].(string); ok && v != "" {
					// use provided root for indexing
					// make it absolute relative to workingDir if needed
					if !strings.HasPrefix(v, "/") {
						v = filepath.Join(c.workingDir, v)
					}
					args["root"] = v
				}
			}

			// handle index operation
			if op == "index" {
				root := c.workingDir
				if args != nil {
					if v, ok := args["root"].(string); ok && v != "" {
						root = v
					}
				}

				idx, err := indexer.IndexRepo(root)
				if err != nil {
					return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
				}

				if err := idx.Save(idxPath); err != nil {
					return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
				}

				msg := fmt.Sprintf("indexed %d chunks and saved to %s", len(idx.Chunks), idxPath)
				return copilot.ToolResult{TextResultForLLM: msg, ResultType: "success"}, nil
			}

			// handle query operation
			var query string
			var k int = 5
			if asStr != "" {
				query = asStr
			}
			if args != nil {
				if v, ok := args["q"].(string); ok && v != "" {
					query = v
				}
				if v, ok := args["query"].(string); ok && v != "" {
					query = v
				}
				if v, ok := args["k"].(float64); ok && int(v) > 0 {
					k = int(v)
				}
				if rb, ok := args["rebuild"].(bool); ok && rb {
					// force rebuild
					idx, err := indexer.IndexRepo(c.workingDir)
					if err != nil {
						return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
					}
					if err := idx.Save(idxPath); err != nil {
						return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
					}
				}
			}

			if query == "" {
				return copilot.ToolResult{ResultType: "error", Error: "empty query"}, fmt.Errorf("empty query")
			}

			// try to load index
			idx, err := indexer.Load(idxPath)
			if err != nil {
				// attempt to build if load failed
				idx, err = indexer.IndexRepo(c.workingDir)
				if err != nil {
					return copilot.ToolResult{ResultType: "error", Error: err.Error()}, err
				}
				_ = idx.Save(idxPath)
			}

			results := idx.Search(query, k)
			if len(results) == 0 {
				return copilot.ToolResult{TextResultForLLM: "no results", ResultType: "text"}, nil
			}

			// format results
			var sb strings.Builder
			for i, r := range results {
				sb.WriteString(fmt.Sprintf("%d) [score=%.4f] %s\n", i+1, r.Score, r.Path))
				sb.WriteString(r.Text)
				sb.WriteString("\n---\n")
			}

			return copilot.ToolResult{TextResultForLLM: sb.String(), ResultType: "text"}, nil
		},
	}

	if err := c.RegisterTool(searchTool); err != nil {
		return err
	}

	return nil
}

// DestroySession destroys the current session and cleans up resources.
func (c *CopilotClient) DestroySession(ctx context.Context) error {
	if c.sdkSession == nil {
		return nil
	}

	_ = c.sdkSession.Destroy()
	c.sdkSession = nil
	return nil
}

// Model returns the configured model name.
func (c *CopilotClient) Model() string {
	return c.model
}

// SendPrompt sends a prompt to the Copilot SDK and returns an event stream.
// The returned channel will be closed when the response is complete.
// An error is returned if there is no active session.
// This method includes automatic retry logic for transient errors.
func (c *CopilotClient) SendPrompt(ctx context.Context, prompt string) (<-chan Event, error) {
	if c.sdkSession == nil {
		return nil, fmt.Errorf("no active session")
	}

	// Create event channel with buffer
	events := make(chan Event, 100)

	// Process prompt asynchronously with retry logic
	go func() {
		defer close(events)
		c.sendPromptWithRetry(ctx, prompt, events)
	}()

	return events, nil
}

// safeEventSender safely sends an event to a channel, recovering from panics if the channel is closed.
// Returns an error if the send failed (e.g., channel closed).
func safeEventSender(events chan<- Event, event Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// Channel was closed, ignore the panic
			err = fmt.Errorf("event channel closed")
		}
	}()

	events <- event
	return nil
}

// sendPromptWithRetry sends the prompt with automatic retry for transient errors.
func (c *CopilotClient) sendPromptWithRetry(ctx context.Context, prompt string, events chan<- Event) {
	var lastErr error

	for attempt := 0; attempt <= len(retryBackoffs); attempt++ {
		// Check for context cancellation before each attempt
		select {
		case <-ctx.Done():
			// Don't send error event here - just return so the channel closes
			// The caller will detect cancellation via ctx.Done()
			return
		default:
		}

		// If this is a retry, wait before trying again
		if attempt > 0 {
			backoff := retryBackoffs[attempt-1]
			select {
			case <-ctx.Done():
				_ = safeEventSender(events, NewErrorEvent(ctx.Err()))
				return
			case <-time.After(backoff):
			}
		}

		// Attempt to send the prompt
		err := c.sendPromptOnce(ctx, prompt, events)
		if err == nil {
			// Success
			return
		}

		lastErr = err

		// Check if error is retryable
		if !isRetryableError(err) {
			_ = safeEventSender(events, NewErrorEvent(err))
			return
		}

		// Error is retryable, will retry on next iteration
	}

	// All retries exhausted
	_ = safeEventSender(events, NewErrorEvent(fmt.Errorf("max retries exceeded: %w", lastErr)))
}

// sendPromptOnce sends the prompt once without retrying.
func (c *CopilotClient) sendPromptOnce(ctx context.Context, prompt string, events chan<- Event) error {
	// Set up done channel to wait for session.idle
	done := make(chan struct{})
	doneOnce := &sync.Once{}
	closeDone := func() {
		doneOnce.Do(func() {
			close(done)
		})
	}

	var sessionErr error
	pendingToolCalls := make(map[string]ToolCall)

	// Subscribe to SDK session events
	unsubscribe := c.sdkSession.On(func(event copilot.SessionEvent) {
		// Check if context is cancelled before processing events
		select {
		case <-ctx.Done():
			// Context cancelled, close done channel to unblock and stop processing
			closeDone()
			return
		default:
		}

		if event.Type == "session.error" && event.Data.Message != nil {
			sessionErr = fmt.Errorf("SDK error: %s", *event.Data.Message)
		}

		c.handleSDKEvent(event, events, closeDone, pendingToolCalls)
	})

	defer unsubscribe()

	// Build message options and send the message
	msgOpts := copilot.MessageOptions{
		Prompt: prompt,
	}

	_, err := c.sdkSession.Send(msgOpts)
	if err != nil {
		// Attempt to include the serialized payload to aid debugging of invalid_request_body
		if b, jbErr := json.Marshal(msgOpts); jbErr == nil {
			return fmt.Errorf("failed to send message: %w; payload=%s", err, string(b))
		}

		return fmt.Errorf("failed to send message: %w", err)
	}

	// Wait for session to become idle or context cancellation
	select {
	case <-ctx.Done():
		// Abort the session and close done to unblock any waiting
		go func() {
			_ = c.sdkSession.Abort()
		}()

		closeDone()
		return ctx.Err()
	case <-done:
		// Response complete - check for session error
		if sessionErr != nil {
			return sessionErr
		}
	}

	return nil
}

// handleSDKEvent processes events from the Copilot SDK and forwards them.
// Uses safeEventSender to protect against writing to closed channels.
func (c *CopilotClient) handleSDKEvent(sdkEvent copilot.SessionEvent, events chan<- Event, closeDone func(), pendingToolCalls map[string]ToolCall) {
	switch sdkEvent.Type {
	case "assistant.message_delta", "assistant.reasoning_delta":
		if sdkEvent.Data.DeltaContent == nil {
			return
		}

		_ = safeEventSender(events, NewTextEvent(*sdkEvent.Data.DeltaContent, strings.Contains(string(sdkEvent.Type), "reasoning")))

	case "assistant.message", "assistant.reasoning":
		// Complete assistant message
		if sdkEvent.Data.Content == nil {
			return
		}

		_ = safeEventSender(events, NewTextEvent(*sdkEvent.Data.Content, strings.Contains(string(sdkEvent.Type), "reasoning")))

	case "tool.execution_start":
		// Tool execution started - the SDK handles this internally
		// We just track it for logging/UI purposes and to match with completion events
		if sdkEvent.Data.ToolName == nil {
			return
		}

		toolCall := ToolCall{
			Name: *sdkEvent.Data.ToolName,
		}

		if sdkEvent.Data.ToolCallID != nil {
			toolCall.ID = *sdkEvent.Data.ToolCallID
			// Store for matching with completion event
			pendingToolCalls[toolCall.ID] = toolCall
		}

		// Type assert Arguments to map[string]interface{} if possible
		if args, ok := sdkEvent.Data.Arguments.(map[string]any); ok {
			toolCall.Parameters = args
		}

		_ = safeEventSender(events, NewToolCallEvent(toolCall))

	case "tool.execution_complete":
		// Tool execution completed - emit result event with actual result from SDK
		var toolCall ToolCall
		if sdkEvent.Data.ToolCallID != nil {
			if tc, ok := pendingToolCalls[*sdkEvent.Data.ToolCallID]; ok {
				toolCall = tc
				delete(pendingToolCalls, *sdkEvent.Data.ToolCallID)
			}
		}

		if toolCall.Name == "" && sdkEvent.Data.ToolName != nil {
			toolCall.Name = *sdkEvent.Data.ToolName
		}

		var result string
		var toolErr error

		if sdkEvent.Data.Result != nil {
			result = sdkEvent.Data.Result.Content
		}

		if sdkEvent.Data.Success != nil && !*sdkEvent.Data.Success {
			if sdkEvent.Data.Error != nil {
				// ErrorUnion can be either ErrorClass or String
				if sdkEvent.Data.Error.ErrorClass != nil {
					toolErr = fmt.Errorf("%s", sdkEvent.Data.Error.ErrorClass.Message)
				} else if sdkEvent.Data.Error.String != nil {
					toolErr = fmt.Errorf("%s", *sdkEvent.Data.Error.String)
				}
			}
		}

		_ = safeEventSender(events, NewToolResultEvent(toolCall, result, toolErr))

	case "session.idle":
		// Session has finished processing
		closeDone()

	case "session.error":
		// Error event - include as much structured information as available
		var parts []string

		if sdkEvent.Data.Message != nil && *sdkEvent.Data.Message != "" {
			parts = append(parts, fmt.Sprintf("message: %s", *sdkEvent.Data.Message))
		}

		if sdkEvent.Data.Error != nil {
			if sdkEvent.Data.Error.ErrorClass != nil {
				ec := sdkEvent.Data.Error.ErrorClass
				if ec.Message != "" {
					parts = append(parts, fmt.Sprintf("error_class: %s", ec.Message))
				}
				if ec.Code != nil && *ec.Code != "" {
					parts = append(parts, fmt.Sprintf("error_code: %s", *ec.Code))
				}
			} else if sdkEvent.Data.Error.String != nil {
				parts = append(parts, fmt.Sprintf("error: %s", *sdkEvent.Data.Error.String))
			}
		}

		if sdkEvent.Data.Stack != nil && *sdkEvent.Data.Stack != "" {
			parts = append(parts, fmt.Sprintf("stack: %s", *sdkEvent.Data.Stack))
		}

		// If we have no useful structured fields, include the raw data JSON for debugging
		var errMsg string
		if len(parts) > 0 {
			errMsg = strings.Join(parts, "; ")
		} else {
			if b, err := json.Marshal(sdkEvent.Data); err == nil {
				errMsg = fmt.Sprintf("raw: %s", string(b))
			} else {
				errMsg = "session.error (no message)"
			}
		}

		_ = safeEventSender(events, NewErrorEvent(fmt.Errorf("SDK error: %s", errMsg)))
	}
}
