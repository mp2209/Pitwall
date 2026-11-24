// Package agent is a minimal coding agent: read files, run the tests, edit,
// repeat. The harness benchmark runs it through the gateway because, unlike
// an off-the-shelf agent, every harness setting is under our control: the
// model, the context strategy, and the task label sent on each call.
//
// It has no shell tool. The model can read and write files inside the task
// directory and run one fixed test command, nothing else.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Config is one agent run.
type Config struct {
	GatewayURL string   // for example http://localhost:8080
	APIKey     string   // a Pitwall virtual key
	Model      string   // any model the gateway routes
	Dir        string   // the task's repository
	Prompt     string   // what to do
	TestCmd    []string // run in Dir; exit 0 means solved
	MaxTurns   int
	MaxTokens  int64
	// KeepToolResults, if above zero, trims all but the newest N tool
	// results to a short marker before each call (the "old tool outputs
	// trimmed" context strategy). Zero keeps the full history.
	KeepToolResults int
	// TrimBatch moves the trim point in steps of this many results instead
	// of one, so up to TrimBatch-1 extra results stay untrimmed. Moving it
	// every turn changes an older message on every call, which invalidates
	// the previous call's prompt-cache entry: only the tools and system
	// prompt are read back. Moving it in batches leaves the history
	// unchanged between steps, so those calls read the cache. 0 or 1 trims
	// every turn.
	TrimBatch   int
	Task        string // x-pitwall-task label
	ConfigLabel string // x-pitwall-config label
	TestTimeout time.Duration
}

// Result summarizes a run.
type Result struct {
	Task       string  `json:"task"`
	TaskID     string  `json:"task_id"` // from the gateway's x-pitwall-task-id
	Model      string  `json:"model"`
	Config     string  `json:"config"`
	Turns      int     `json:"turns"`
	StopReason string  `json:"stop_reason"`
	Outcome    string  `json:"outcome"` // solved, failed or stopped
	Seconds    float64 `json:"seconds"`
	Error      string  `json:"error,omitempty"`
}

const systemPrompt = `You are a coding agent working in a small repository.
Use the tools to look at the code, make the smallest correct change, and run the tests.
Keep going until the tests pass, then stop and reply with one sentence saying what you changed.
Do not modify the tests unless the task says to.`

// history is the conversation in a form that can be re-rendered each turn,
// so old tool output can be trimmed without losing the original.
type turn struct {
	assistant *anthropic.MessageParam
	results   []toolResult
}

type toolResult struct {
	id, content string
	isError     bool
}

// Run runs the agent until it stops calling tools, hits MaxTurns, or the
// gateway refuses a call, then runs the tests itself and reports the outcome.
func Run(ctx context.Context, cfg Config) Result {
	start := time.Now()
	if cfg.MaxTurns == 0 {
		cfg.MaxTurns = 30
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = 8192
	}
	if cfg.TestTimeout == 0 {
		cfg.TestTimeout = 2 * time.Minute
	}
	res := Result{Task: cfg.Task, Model: cfg.Model, Config: cfg.ConfigLabel}
	var taskID string
	client := anthropic.NewClient(
		option.WithBaseURL(cfg.GatewayURL),
		option.WithAPIKey(cfg.APIKey),
		option.WithHeader("x-pitwall-task", cfg.Task),
		option.WithHeader("x-pitwall-agent", "pitwall-agent"),
		option.WithHeader("x-pitwall-config", cfg.ConfigLabel),
		// A retry is another billed call; the benchmark wants one call per
		// turn, and a refusal from the gateway should end the run at once.
		option.WithMaxRetries(0),
		option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			resp, err := next(req)
			if resp != nil && resp.Header.Get("x-pitwall-task-id") != "" {
				taskID = resp.Header.Get("x-pitwall-task-id")
			}
			return resp, err
		}),
	)
	tools := toolDefs()
	var turns []turn
	stopped := false
	for res.Turns < cfg.MaxTurns {
		res.Turns++
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     cfg.Model,
			MaxTokens: cfg.MaxTokens,
			System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
			Messages:  render(cfg.Prompt, turns, cfg.KeepToolResults, cfg.TrimBatch),
			Tools:     tools,
		})
		if err != nil {
			// The gateway refusing a call (budget, runaway guard) or a
			// provider error ends the run.
			res.Error, res.StopReason, stopped = err.Error(), "error", true
			break
		}
		res.StopReason = string(resp.StopReason)
		assistant := resp.ToParam()
		t := turn{assistant: &assistant}
		for _, block := range resp.Content {
			if use, ok := block.AsAny().(anthropic.ToolUseBlock); ok {
				out, isErr := runTool(ctx, cfg, use.Name, []byte(use.JSON.Input.Raw()))
				t.results = append(t.results, toolResult{id: use.ID, content: out, isError: isErr})
			}
		}
		turns = append(turns, t)
		if resp.StopReason != anthropic.StopReasonToolUse {
			break
		}
	}
	res.TaskID = taskID

	// The agent's word isn't the outcome; the tests are.
	_, testErr := runTests(ctx, cfg)
	switch {
	case testErr == nil:
		res.Outcome = "solved"
	case stopped:
		res.Outcome = "stopped"
	default:
		res.Outcome = "failed"
	}
	if err := reportOutcome(ctx, cfg, res.Outcome); err != nil && res.Error == "" {
		res.Error = "reporting outcome: " + err.Error()
	}
	res.Seconds = time.Since(start).Seconds()
	return res
}

// render builds the message list, trimming old tool results if asked: the
// first cutoff results (counting all of them) are candidates for trimming.
func render(prompt string, turns []turn, keep, batch int) []anthropic.MessageParam {
	msgs := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}
	total := 0
	for _, t := range turns {
		total += len(t.results)
	}
	cutoff := 0
	if keep > 0 && total > keep {
		cutoff = total - keep
		if batch > 1 {
			cutoff -= cutoff % batch
		}
	}
	seen := 0
	for _, t := range turns {
		msgs = append(msgs, *t.assistant)
		if len(t.results) == 0 {
			continue
		}
		blocks := make([]anthropic.ContentBlockParamUnion, 0, len(t.results))
		for _, r := range t.results {
			seen++
			content := r.content
			if seen <= cutoff && len(content) > 200 {
				content = fmt.Sprintf("[older tool output trimmed: %d bytes]", len(r.content))
			}
			blocks = append(blocks, anthropic.NewToolResultBlock(r.id, content, r.isError))
		}
		msgs = append(msgs, anthropic.NewUserMessage(blocks...))
	}
	return msgs
}

func toolDefs() []anthropic.ToolUnionParam {
	str := map[string]any{"type": "string"}
	defs := []anthropic.ToolParam{
		{Name: "run_tests", Description: anthropic.String("Run the repository's tests. Returns the exit code and output."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{}}},
		{Name: "list_files", Description: anthropic.String("List files under a directory of the repository (default: the root)."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"path": str}}},
		{Name: "read_file", Description: anthropic.String("Read a file of the repository."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"path": str}, Required: []string{"path"}}},
		{Name: "write_file", Description: anthropic.String("Create or overwrite a file with the given content."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"path": str, "content": str}, Required: []string{"path", "content"}}},
		{Name: "edit_file", Description: anthropic.String("Replace one exact occurrence of old_text with new_text in a file."),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: map[string]any{"path": str, "old_text": str, "new_text": str},
				Required: []string{"path", "old_text", "new_text"}}},
	}
	out := make([]anthropic.ToolUnionParam, len(defs))
	for i := range defs {
		out[i] = anthropic.ToolUnionParam{OfTool: &defs[i]}
	}
	return out
}

const maxToolOutput = 16 << 10

// runTool executes one tool call. Errors go back to the model as tool
// results with is_error set, so it can correct itself.
func runTool(ctx context.Context, cfg Config, name string, input []byte) (string, bool) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "invalid tool input: " + err.Error(), true
	}
	switch name {
	case "run_tests":
		out, err := runTests(ctx, cfg)
		if err != nil {
			return "tests failed: " + err.Error() + "\n" + out, false
		}
		return "tests passed\n" + out, false
	case "list_files":
		return listFiles(cfg.Dir, in.Path)
	case "read_file":
		p, err := safePath(cfg.Dir, in.Path)
		if err != nil {
			return err.Error(), true
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err.Error(), true
		}
		return clip(string(b)), false
	case "write_file":
		p, err := safePath(cfg.Dir, in.Path)
		if err != nil {
			return err.Error(), true
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err.Error(), true
		}
		if err := os.WriteFile(p, []byte(in.Content), 0o644); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), false
	case "edit_file":
		p, err := safePath(cfg.Dir, in.Path)
		if err != nil {
			return err.Error(), true
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err.Error(), true
		}
		switch n := strings.Count(string(b), in.OldText); {
		case in.OldText == "" || n == 0:
			return "old_text not found in " + in.Path, true
		case n > 1:
			return fmt.Sprintf("old_text appears %d times in %s; include more context so it is unique", n, in.Path), true
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), in.OldText, in.NewText, 1)), 0o644); err != nil {
			return err.Error(), true
		}
		return "edited " + in.Path, false
	}
	return "unknown tool " + name, true
}

// safePath resolves a model-supplied path inside dir, refusing anything that
// escapes it, including through symlinks.
func safePath(dir, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path must be relative to the repository root, got %q", rel)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, filepath.Clean("/" + rel)[1:])
	// Check the deepest existing ancestor, so a symlinked directory can't
	// lead outside.
	existing := p
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		existing = filepath.Dir(existing)
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	if real != root && !strings.HasPrefix(real, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the repository", rel)
	}
	return p, nil
}

func listFiles(dir, rel string) (string, bool) {
	start := dir
	if rel != "" && rel != "." {
		p, err := safePath(dir, rel)
		if err != nil {
			return err.Error(), true
		}
		start = p
	}
	var out []string
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			r, _ := filepath.Rel(dir, p)
			out = append(out, r)
		}
		if len(out) >= 500 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return err.Error(), true
	}
	return strings.Join(out, "\n"), false
}

// runTests runs the fixed test command; a nil error means it exited 0.
func runTests(ctx context.Context, cfg Config) (string, error) {
	if len(cfg.TestCmd) == 0 {
		return "", errors.New("no test command configured")
	}
	tctx, cancel := context.WithTimeout(ctx, cfg.TestTimeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, cfg.TestCmd[0], cfg.TestCmd[1:]...)
	cmd.Dir = cfg.Dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := buf.String()
	if len(out) > maxToolOutput {
		out = "…" + out[len(out)-maxToolOutput:] // the end of test output is what matters
	}
	return out, err
}

func clip(s string) string {
	if len(s) > maxToolOutput*4 {
		return s[:maxToolOutput*4] + fmt.Sprintf("\n…[%d more bytes]", len(s)-maxToolOutput*4)
	}
	return s
}

// reportOutcome tells the gateway how the task ended.
func reportOutcome(ctx context.Context, cfg Config, outcome string) error {
	body, _ := json.Marshal(map[string]string{"task": cfg.Task, "outcome": outcome})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.GatewayURL, "/")+"/v1/pitwall/tasks/outcome", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		return nil // dev-mode gateway without a database: nothing to record
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway answered %s", resp.Status)
	}
	return nil
}
