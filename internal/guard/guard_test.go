package guard

import (
	"strings"
	"testing"

	"github.com/mp2209/pitwall/internal/metering"
	"github.com/mp2209/pitwall/internal/tasks"
)

func TestCheck(t *testing.T) {
	c := Defaults()
	cases := []struct {
		name   string
		in     Input
		action string
		events string
	}{
		{"quiet", Input{Loop: tasks.LoopStats{RepeatedExchanges: 2, RepeatedToolCalls: 2}}, ActionNone, ""},
		{"stuck warns", Input{Loop: tasks.LoopStats{RepeatedExchanges: 3, RepeatedToolCalls: 3}}, ActionWarn, "repeated_exchange:warn"},
		{"stuck stops", Input{Loop: tasks.LoopStats{RepeatedExchanges: 5, RepeatedToolCalls: 5}}, ActionStop, "repeated_exchange:stop"},
		{"polling warns late", Input{Loop: tasks.LoopStats{RepeatedExchanges: 1, RepeatedToolCalls: 6}}, ActionWarn, "repeated_tool_call:warn"},
		{"context", Input{ContextTokens: 600_000}, ActionStop, "context_size:stop"},
		{"cost warn and loop stop", Input{Loop: tasks.LoopStats{RepeatedExchanges: 5, RepeatedToolCalls: 5}, TaskCost: metering.FromUSD(8.5)},
			ActionStop, "repeated_exchange:stop,task_cost:warn"},
	}
	for _, tc := range cases {
		v := c.Check(tc.in)
		if v.Action != tc.action || strings.Join(v.Events(), ",") != tc.events {
			t.Errorf("%s: action %q events %v, want %q %q", tc.name, v.Action, v.Events(), tc.action, tc.events)
		}
	}
	off := Defaults()
	off.Enabled = false
	if v := off.Check(Input{ContextTokens: 1 << 30}); v.Action != ActionNone {
		t.Error("a disabled guard never acts")
	}
	partial := Defaults()
	partial.TaskCostUSD = Limit{Warn: 1} // warn only
	if v := partial.Check(Input{TaskCost: metering.FromUSD(100)}); v.Action != ActionWarn {
		t.Errorf("a limit with no stop step only warns: %q", v.Action)
	}
}

func TestMessages(t *testing.T) {
	v := Defaults().Check(Input{Loop: tasks.LoopStats{RepeatedExchanges: 5, RepeatedToolCalls: 5, LastToolCall: "run_tests {}"},
		TaskCost: metering.FromUSD(8.25)})
	msg := v.StopMessage()
	for _, want := range []string{"Pitwall stopped this call", "same step 5 times", "run_tests {}", "Change the approach"} {
		if !strings.Contains(msg, want) {
			t.Errorf("stop message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "cost") {
		t.Error("the stop message gives only the reasons that stopped the call")
	}
	if h := v.WarningHeader(); h != "repeated_exchange=5 (stops at 5); task_cost=8.25 (stops at 10)" {
		t.Errorf("warning header = %q", h)
	}
}
