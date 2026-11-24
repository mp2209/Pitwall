package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/mp2209/pitwall/internal/api"
	"github.com/mp2209/pitwall/internal/config"
	"github.com/mp2209/pitwall/mockprovider"
)

func TestSafePathStaysInside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "/etc/passwd", "escape/x.go", "escape"} {
		if _, err := safePath(root, bad); err == nil {
			t.Errorf("safePath(%q) should be refused", bad)
		}
	}
	// ".." is clamped to the root rather than allowed out.
	p, err := safePath(root, "../../etc/passwd")
	if err != nil || !strings.HasPrefix(p, mustEval(t, root)) {
		t.Errorf("../ path resolved to %q (%v)", p, err)
	}
	if _, err := safePath(root, "new/dir/file.go"); err != nil {
		t.Errorf("a new file inside the repo should be allowed: %v", err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFileTools(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Dir: dir}
	ctx := context.Background()
	if out, isErr := runTool(ctx, cfg, "write_file", []byte(`{"path":"pkg/a.go","content":"x := 1\ny := 1\n"}`)); isErr {
		t.Fatal(out)
	}
	if out, isErr := runTool(ctx, cfg, "edit_file", []byte(`{"path":"pkg/a.go","old_text":"1","new_text":"2"}`)); !isErr || !strings.Contains(out, "2 times") {
		t.Errorf("ambiguous edit: %q", out)
	}
	if out, isErr := runTool(ctx, cfg, "edit_file", []byte(`{"path":"pkg/a.go","old_text":"y := 1","new_text":"y := 2"}`)); isErr {
		t.Errorf("unique edit failed: %q", out)
	}
	if out, _ := runTool(ctx, cfg, "read_file", []byte(`{"path":"pkg/a.go"}`)); out != "x := 1\ny := 2\n" {
		t.Errorf("read after edit = %q", out)
	}
	if out, _ := runTool(ctx, cfg, "list_files", []byte(`{}`)); out != filepath.Join("pkg", "a.go") {
		t.Errorf("list = %q", out)
	}
	if _, isErr := runTool(ctx, cfg, "rm_rf", []byte(`{}`)); !isErr {
		t.Error("unknown tools must fail")
	}
}

func TestRenderTrimsOldToolOutput(t *testing.T) {
	assistant := anthropic.NewAssistantMessage(anthropic.NewTextBlock("ok"))
	long := strings.Repeat("log ", 100)
	turns := []turn{
		{assistant: &assistant, results: []toolResult{{id: "a", content: long}}},
		{assistant: &assistant, results: []toolResult{{id: "b", content: long}}},
		{assistant: &assistant, results: []toolResult{{id: "c", content: long}}},
	}
	count := func(msgs []anthropic.MessageParam) (trimmed int) {
		for _, m := range msgs {
			b, _ := m.MarshalJSON()
			trimmed += strings.Count(string(b), "older tool output trimmed")
		}
		return trimmed
	}
	if n := count(render("p", turns, 0, 0)); n != 0 {
		t.Errorf("full history trimmed %d results", n)
	}
	if n := count(render("p", turns, 1, 0)); n != 2 {
		t.Errorf("keep=1 trimmed %d results, want 2", n)
	}
	// In batches of 2, the trim point moves only when two more results can
	// go: with 3 results and keep=1 that is now (2 trimmed); with 4 it stays.
	if n := count(render("p", turns, 1, 2)); n != 2 {
		t.Errorf("keep=1 batch=2, 3 results: trimmed %d, want 2", n)
	}
	four := make([]turn, 0, 5)
	four = append(four, turns...)
	four = append(four, turn{assistant: &assistant, results: []toolResult{{id: "d", content: long}}})
	if n := count(render("p", four, 1, 2)); n != 2 {
		t.Errorf("keep=1 batch=2, 4 results: trimmed %d, want 2", n)
	}
	if n := count(render("p", append(four, four[0]), 1, 2)); n != 4 {
		t.Errorf("keep=1 batch=2, 5 results: trimmed %d, want 4", n)
	}
	if n := count(render("p", four, 1, 1)); n != 3 {
		t.Errorf("keep=1 batch=1, 4 results: trimmed %d, want 3", n)
	}
}

// A full run against a dev-mode gateway and the mock provider: the mock calls
// the first tool (run_tests) once, then answers, and the agent's own test run
// decides the outcome.
func TestRunThroughGateway(t *testing.T) {
	mock := httptest.NewServer(mockprovider.New(mockprovider.Config{}).Handler())
	defer mock.Close()
	cfg, err := config.Parse([]byte(`
providers:
  - {name: mock, kind: anthropic, base_url: ` + mock.URL + `}
models:
  - {match: mock-claude, provider: mock}
`))
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(api.New(cfg, api.Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler())
	defer gw.Close()

	for _, tc := range []struct {
		test    string
		outcome string
	}{{"true", "solved"}, {"false", "failed"}} {
		res := Run(context.Background(), Config{
			GatewayURL: gw.URL, APIKey: "dev", Model: "mock-claude", Dir: t.TempDir(),
			Prompt: "Make the tests pass", TestCmd: []string{tc.test}, Task: "demo-" + tc.test,
		})
		if res.Outcome != tc.outcome || res.Turns != 2 || res.StopReason != "end_turn" || res.Error != "" {
			t.Errorf("%s: result = %+v", tc.test, res)
		}
		if !strings.HasPrefix(res.TaskID, "t_") {
			t.Errorf("task id from the gateway = %q", res.TaskID)
		}
	}

	// A model the gateway doesn't route ends the run as stopped.
	res := Run(context.Background(), Config{GatewayURL: gw.URL, APIKey: "dev", Model: "nope", Dir: t.TempDir(),
		Prompt: "x", TestCmd: []string{"false"}, Task: "demo-stop"})
	if res.Outcome != "stopped" || res.Error == "" || res.Turns != 1 {
		t.Errorf("refused run = %+v", res)
	}
}
