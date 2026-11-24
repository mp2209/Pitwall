// Command agent runs Pitwall's minimal coding agent on one task directory,
// through the gateway, and prints a JSON summary.
//
//	PITWALL_KEY=pw-... agent -model claude-sonnet-5-5 -dir bench/tasks/001-word-count
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/mp2209/pitwall/agent"
)

func main() {
	gateway := flag.String("gateway", envOr("PITWALL_GATEWAY_URL", "http://localhost:8080"), "gateway base URL")
	model := flag.String("model", "mock-claude", "model name, as routed by the gateway")
	dir := flag.String("dir", ".", "task repository")
	prompt := flag.String("prompt", "", "task description (default: the directory's TASK.md)")
	testCmd := flag.String("test-cmd", "go test ./...", "test command, run in -dir; exit 0 means solved")
	maxTurns := flag.Int("max-turns", 30, "most model calls before giving up")
	maxTokens := flag.Int64("max-tokens", 8192, "max_tokens per call")
	keep := flag.Int("keep-tool-results", 0, "trim all but the newest N tool results (0 keeps full history)")
	trimBatch := flag.Int("trim-batch", 1, "move the trim point in steps of this many results, keeping the cached prefix stable in between")
	task := flag.String("task", "", "task label sent as x-pitwall-task (default: directory name plus a unique suffix)")
	config := flag.String("config", "", "configuration label sent as x-pitwall-config")
	flag.Parse()

	key := os.Getenv("PITWALL_KEY")
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fail(err)
	}
	if *prompt == "" {
		b, err := os.ReadFile(filepath.Join(abs, "TASK.md"))
		if err != nil {
			fail(fmt.Errorf("no -prompt and no TASK.md in %s", abs))
		}
		*prompt = string(b)
	}
	if *task == "" {
		var b [3]byte
		_, _ = rand.Read(b[:])
		*task = fmt.Sprintf("%s-%s-%s", filepath.Base(abs), time.Now().UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res := agent.Run(ctx, agent.Config{
		GatewayURL: *gateway, APIKey: key, Model: *model, Dir: abs, Prompt: *prompt,
		TestCmd: strings.Fields(*testCmd), MaxTurns: *maxTurns, MaxTokens: *maxTokens,
		KeepToolResults: *keep, TrimBatch: *trimBatch, Task: *task, ConfigLabel: *config,
	})
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "agent:", err)
	os.Exit(2)
}
