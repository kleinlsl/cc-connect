package acp

import (
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

func TestNew_DisplayNameDefault(t *testing.T) {
	a, err := New(map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)
	if got := agent.CLIDisplayName(); got != "ACP" {
		t.Fatalf("CLIDisplayName = %q, want ACP", got)
	}
}

func TestNew_DisplayNameCustom(t *testing.T) {
	a, err := New(map[string]any{
		"command":      "true",
		"display_name": "Copilot ACP",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)
	if got := agent.CLIDisplayName(); got != "Copilot ACP" {
		t.Fatalf("CLIDisplayName = %q, want Copilot ACP", got)
	}
}

func TestWorkspaceAgentOptions(t *testing.T) {
	a, err := New(map[string]any{
		"command":          "true",
		"args":             []any{"--acp", "--stdio"},
		"env":              map[string]any{"FOO": "bar", "COPILOT_VALUE": "a=b"},
		"auth_method":      "cursor_login",
		"display_name":     "Copilot ACP",
		"compress_command": "/compress",
		"model_command":    "/model",
	})
	if err != nil {
		t.Fatal(err)
	}

	agent := a.(*Agent)
	agent.SetSessionEnv([]string{"SESSION_ONLY=1"})

	snapshotter, ok := a.(core.WorkspaceAgentOptionSnapshotter)
	if !ok {
		t.Fatalf("agent does not implement WorkspaceAgentOptionSnapshotter")
	}
	opts := snapshotter.WorkspaceAgentOptions()

	if got, _ := opts["cmd"].(string); got != "true" {
		t.Fatalf("cmd = %q, want true", got)
	}
	gotArgs, _ := opts["args"].([]string)
	if len(gotArgs) != 2 || gotArgs[0] != "--acp" || gotArgs[1] != "--stdio" {
		t.Fatalf("args = %#v, want [--acp --stdio]", gotArgs)
	}
	gotEnv, _ := opts["env"].(map[string]string)
	if len(gotEnv) != 2 || gotEnv["FOO"] != "bar" || gotEnv["COPILOT_VALUE"] != "a=b" {
		t.Fatalf("env = %#v, want config env only", gotEnv)
	}
	if got, _ := opts["auth_method"].(string); got != "cursor_login" {
		t.Fatalf("auth_method = %q, want cursor_login", got)
	}
	if got, _ := opts["display_name"].(string); got != "Copilot ACP" {
		t.Fatalf("display_name = %q, want Copilot ACP", got)
	}
	if got, _ := opts["compress_command"].(string); got != "/compress" {
		t.Fatalf("compress_command = %q, want /compress", got)
	}
	if got, _ := opts["model_command"].(string); got != "/model" {
		t.Fatalf("model_command = %q, want /model", got)
	}
}

// TestNew_CompressCommandDefault verifies the generic ACP agent implements
// core.ContextCompressor but reports no compression command unless one is
// configured — so ACP servers without such a command keep the old
// "unsupported" behaviour.
func TestNew_CompressCommandDefault(t *testing.T) {
	a, err := New(map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	compressor, ok := a.(core.ContextCompressor)
	if !ok {
		t.Fatalf("acp Agent does not implement core.ContextCompressor")
	}
	if got := compressor.CompressCommand(); got != "" {
		t.Fatalf("CompressCommand = %q, want empty by default", got)
	}
}

// TestNew_CompressCommandCustom verifies a configured command is trimmed and
// returned (Hermes uses /compress rather than /compact).
func TestNew_CompressCommandCustom(t *testing.T) {
	a, err := New(map[string]any{
		"command":          "true",
		"compress_command": "  /compress  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	compressor := a.(core.ContextCompressor)
	if got := compressor.CompressCommand(); got != "/compress" {
		t.Fatalf("CompressCommand = %q, want /compress (trimmed)", got)
	}
}

// TestNew_ModelCommandDefault verifies the generic ACP agent implements
// core.ModelCommand but reports no model command unless configured, so ACP
// servers without such a command keep the structured / "not supported" path.
func TestNew_ModelCommandDefault(t *testing.T) {
	a, err := New(map[string]any{"command": "true"})
	if err != nil {
		t.Fatal(err)
	}
	passer, ok := a.(core.ModelCommand)
	if !ok {
		t.Fatalf("acp Agent does not implement core.ModelCommand")
	}
	if got := passer.ModelCommand(); got != "" {
		t.Fatalf("ModelCommand = %q, want empty by default", got)
	}
}

// TestNew_ModelCommandCustom verifies a configured model command is trimmed
// and returned (Hermes uses /model over session/prompt).
func TestNew_ModelCommandCustom(t *testing.T) {
	a, err := New(map[string]any{
		"command":       "true",
		"model_command": "  /model  ",
	})
	if err != nil {
		t.Fatal(err)
	}
	passer := a.(core.ModelCommand)
	if got := passer.ModelCommand(); got != "/model" {
		t.Fatalf("ModelCommand = %q, want /model (trimmed)", got)
	}
}
