// Package guard spots a runaway agent before it burns its budget.
//
// It checks four signals before each call of a task:
//
//   - repeated_exchange: the agent repeated the same step and got the same
//     result several times in a row. This is the stuck agent: running the
//     same failing command and getting the same failure.
//   - repeated_tool_call: the same tool call with the same arguments several
//     times in a row, whatever came back. Polling can look like this, so
//     its limits are higher.
//   - context_size: the prompt passed a size threshold.
//   - task_cost: the task's cost so far passed its cap.
//
// Each signal has two limits. Reaching the warn limit adds an
// x-pitwall-warning header to the response; reaching the stop limit refuses
// the call with an error message the agent shows its user. An agent that
// changes approach drops below the limits and may carry on; the guard keeps
// no verdict between calls, it re-reads the conversation each time.
//
// "Stuck" versus "legitimately retrying": a retry that changes something
// (an edit between test runs, a different result) breaks the run of
// identical exchanges, because the history now differs. Only repetition
// with nothing changing counts toward the low limits.
package guard

import (
	"fmt"
	"strings"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/tasks"
)

// Signal names.
const (
	RepeatedExchange = "repeated_exchange"
	RepeatedToolCall = "repeated_tool_call"
	ContextSize      = "context_size"
	TaskCost         = "task_cost"
)

// Actions, in increasing severity.
const (
	ActionNone = ""
	ActionWarn = "warn"
	ActionStop = "stop"
)

// Limit is a signal's warn and stop thresholds; 0 turns that step off.
type Limit struct {
	Warn float64 `yaml:"warn"`
	Stop float64 `yaml:"stop"`
}

// Config holds the guard's limits.
type Config struct {
	Enabled          bool
	RepeatedExchange Limit // consecutive identical exchanges
	RepeatedToolCall Limit // consecutive identical tool calls
	ContextTokens    Limit // estimated prompt tokens
	TaskCostUSD      Limit // task cost so far, USD
}

// Defaults: three identical exchanges warn and five stop; tool calls get
// more room (6 and 10) because polling repeats calls legitimately; context
// warns at 200k tokens and stops at 500k; a task warns at $8 and stops at $10.
func Defaults() Config {
	return Config{
		Enabled:          true,
		RepeatedExchange: Limit{Warn: 3, Stop: 5},
		RepeatedToolCall: Limit{Warn: 6, Stop: 10},
		ContextTokens:    Limit{Warn: 200_000, Stop: 500_000},
		TaskCostUSD:      Limit{Warn: 8, Stop: 10},
	}
}

// Input is what the guard knows about a call.
type Input struct {
	Loop          tasks.LoopStats
	ContextTokens int64
	TaskCost      metering.Nanos // the task's cost before this call
}

// Finding is one signal past a limit.
type Finding struct {
	Signal string
	Action string
	Value  float64
	Limit  float64 // the limit that was reached
	Stop   float64 // the stop limit, for warnings
	Reason string  // for people
}

// Verdict is the guard's decision for one call.
type Verdict struct {
	Action   string
	Findings []Finding
}

func (l Limit) action(v float64) (string, float64) {
	switch {
	case l.Stop > 0 && v >= l.Stop:
		return ActionStop, l.Stop
	case l.Warn > 0 && v >= l.Warn:
		return ActionWarn, l.Warn
	}
	return ActionNone, 0
}

// Check evaluates the signals for one call.
func (c Config) Check(in Input) Verdict {
	var v Verdict
	if !c.Enabled {
		return v
	}
	add := func(signal string, value float64, limit Limit, reason string) {
		action, reached := limit.action(value)
		if action == ActionNone {
			return
		}
		v.Findings = append(v.Findings, Finding{Signal: signal, Action: action, Value: value, Limit: reached, Stop: limit.Stop, Reason: reason})
		if action == ActionStop || v.Action == ActionNone {
			v.Action = action
		}
	}
	call := in.Loop.LastToolCall
	if call == "" {
		call = "no tool call"
	}
	add(RepeatedExchange, float64(in.Loop.RepeatedExchanges), c.RepeatedExchange,
		fmt.Sprintf("the agent repeated the same step %d times in a row with the same result (%s)", in.Loop.RepeatedExchanges, call))
	add(RepeatedToolCall, float64(in.Loop.RepeatedToolCalls), c.RepeatedToolCall,
		fmt.Sprintf("the agent made the same tool call %d times in a row (%s)", in.Loop.RepeatedToolCalls, call))
	add(ContextSize, float64(in.ContextTokens), c.ContextTokens,
		fmt.Sprintf("the prompt has grown to about %d tokens", in.ContextTokens))
	add(TaskCost, in.TaskCost.USD(), c.TaskCostUSD,
		fmt.Sprintf("this task has cost $%.2f so far", in.TaskCost.USD()))
	return v
}

// Events lists the findings as "signal:action", for records and metrics.
func (v Verdict) Events() []string {
	out := make([]string, len(v.Findings))
	for i, f := range v.Findings {
		out[i] = f.Signal + ":" + f.Action
	}
	return out
}

// WarningHeader is the x-pitwall-warning value for a warn verdict.
func (v Verdict) WarningHeader() string {
	var parts []string
	for _, f := range v.Findings {
		p := fmt.Sprintf("%s=%s", f.Signal, trimFloat(f.Value))
		if f.Stop > 0 {
			p += fmt.Sprintf(" (stops at %s)", trimFloat(f.Stop))
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// StopMessage is the error the agent shows its user when a call is refused.
func (v Verdict) StopMessage() string {
	var reasons []string
	for _, f := range v.Findings {
		if f.Action == ActionStop {
			reasons = append(reasons, f.Reason)
		}
	}
	return "Pitwall stopped this call: " + strings.Join(reasons, "; ") +
		". This looks like a runaway agent. Change the approach, or ask a Pitwall admin to raise the guard limits."
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.2f", f)
}
