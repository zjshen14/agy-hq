package transcript

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSteps_ParallelFIFOPairingAndMinStepIdx(t *testing.T) {
	txPath := filepath.Join(t.TempDir(), "tx.jsonl")
	lines := []string{
		`{"step_index":0,"type":"USER_INPUT","content":"turn 1"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command","args":{"CommandLine":"cmd-old"}}]}`,
		`{"step_index":2,"type":"GENERIC","status":"ERROR","content":"old turn error"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command","args":{"CommandLine":"cmd-first"}},{"name":"read_file","args":{"Path":"/tmp/a.go"}}]}`,
		`{"step_index":4,"type":"GENERIC","status":"DONE","content":"Created At: 2026-11-25T00:00:00Z\nCompleted At: 2026-11-25T00:00:01Z\nCommand exited with code 2.\nunknown flag: --bad"}`,
		`{"step_index":5,"type":"GENERIC","status":"DONE","content":"Showing lines 1-5 of 5\nCommand exited with code 2.\n"}`,
	}
	if err := os.WriteFile(txPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	// D2: minStepIdx=3 must skip turn 1 (steps 0..2)
	steps, tainted, err := ParseSteps(txPath, 3)
	if err != nil {
		t.Fatalf("ParseSteps: %v", err)
	}
	if tainted {
		t.Fatalf("expected untainted transcript")
	}
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps from minStepIdx=3, got %d", len(steps))
	}
	// D1: step 4 pairs with cmd-first (run_command), step 5 pairs with read_file (file viewer, NOT an error)
	if steps[1].ToolName != "run_command" || steps[1].CmdLine != "cmd-first" || !steps[1].IsError {
		t.Fatalf("expected step 4 paired with run_command error, got: %+v", steps[1])
	}
	if steps[2].ToolName != "read_file" || steps[2].IsError {
		t.Fatalf("expected step 5 paired with read_file non-error, got: %+v", steps[2])
	}
	ex := FirstLineExcerpt(steps[1].Content, 120)
	if strings.Contains(ex, "Created At:") || !strings.Contains(ex, "unknown flag") {
		t.Fatalf("expected timestamp prefix stripped from excerpt, got %q", ex)
	}
}

func TestParseSteps_QuarantinedReadMarksTainted(t *testing.T) {
	txPath := filepath.Join(t.TempDir(), "tx-taint.jsonl")
	lines := []string{
		`{"step_index":0,"type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command","args":{"CommandLine":"hq gmail read-quarantined --id=123"}}]}`,
		`{"step_index":1,"type":"GENERIC","status":"DONE","content":"quarantined body"}`,
	}
	_ = os.WriteFile(txPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	_, tainted, err := ParseSteps(txPath, 0)
	if err != nil {
		t.Fatalf("ParseSteps: %v", err)
	}
	if !tainted {
		t.Fatalf("expected quarantined read command to mark transcript tainted")
	}
}
