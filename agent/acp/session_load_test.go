package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// mockHandshakeServer replies to the JSON-RPC methods the acp handshake emits.
// It records every method it saw so tests can assert whether session/new ran.
//   - loadResult / loadErr shape the session/load response; when loadErr is set
//     it returns a JSON-RPC error instead of a result.
//   - session/new always returns a fixed brand-new id, which a correct resume
//     path must never reach.
func mockHandshakeServer(t *testing.T, rReq io.Reader, wResp io.Writer, mu *sync.Mutex, methods *[]string,
	loadResult string, loadErr bool) {
	t.Helper()
	sc := bufio.NewScanner(rReq)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var req map[string]any
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		method, _ := req["method"].(string)
		id := req["id"]
		mu.Lock()
		*methods = append(*methods, method)
		mu.Unlock()

		var line string
		switch method {
		case "initialize":
			line = `{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}`
		case "session/load":
			if loadErr {
				line = `{"jsonrpc":"2.0","id":%v,"error":{"code":-32000,"message":"load boom"}}`
			} else {
				line = fmt.Sprintf(`{"jsonrpc":"2.0","id":%%v,"result":%s}`, loadResult)
			}
		case "session/new":
			line = `{"jsonrpc":"2.0","id":%v,"result":{"sessionId":"brand-new-id"}}`
		default:
			line = `{"jsonrpc":"2.0","id":%v,"result":{}}`
		}
		_, _ = io.WriteString(wResp, fmt.Sprintf(line+"\n", id))
	}
}

func containsMethod(methods []string, want string) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// Regression: per ACP spec LoadSessionResponse has NO sessionId (the loaded
// session is the one requested). Before the fix, handshake required a non-empty
// response sessionId and silently fell through to session/new, orphaning the
// session the agent had just restored from disk. This test fails on the old
// code (ends on "brand-new-id" and observes session/new) and passes after.
func TestHandshake_ResumeLoadWithoutSessionID_KeepsRequestedSession(t *testing.T) {
	cb := &fakeCallbacks{}
	s, wResp, rReq := newTestSession(t, cb)
	s.acpSessID = "" // start clean; handshake must establish the id.

	var mu sync.Mutex
	var methods []string
	// Spec-compliant load response: modes only, no sessionId.
	go mockHandshakeServer(t, rReq, wResp, &mu, &methods,
		`{"modes":{"currentModeId":"default","availableModes":[]}}`, false)

	const resumeID = "restored-session-uuid"
	if err := s.handshake(resumeID, ""); err != nil {
		t.Fatalf("handshake resume: %v", err)
	}
	if got := s.currentACPSessionID(); got != resumeID {
		t.Fatalf("acp session id = %q, want requested resume id %q (must not create a new session)", got, resumeID)
	}
	mu.Lock()
	defer mu.Unlock()
	if containsMethod(methods, "session/new") {
		t.Fatalf("session/new must not be called after a successful session/load, methods=%v", methods)
	}
	if !containsMethod(methods, "session/load") {
		t.Fatalf("expected session/load to be called, methods=%v", methods)
	}
}

// A non-standard agent that echoes a different sessionId on load must win.
func TestHandshake_ResumeLoadEchoesSessionID_UsesEchoedID(t *testing.T) {
	cb := &fakeCallbacks{}
	s, wResp, rReq := newTestSession(t, cb)
	s.acpSessID = ""

	var mu sync.Mutex
	var methods []string
	go mockHandshakeServer(t, rReq, wResp, &mu, &methods,
		`{"sessionId":"echoed-id","modes":{"currentModeId":"default","availableModes":[]}}`, false)

	if err := s.handshake("requested-id", ""); err != nil {
		t.Fatalf("handshake resume: %v", err)
	}
	if got := s.currentACPSessionID(); got != "echoed-id" {
		t.Fatalf("acp session id = %q, want echoed-id", got)
	}
	if containsMethod(methods, "session/new") {
		t.Fatalf("session/new must not be called, methods=%v", methods)
	}
}

// When session/load actually errors, the existing fallback to session/new
// must keep working.
func TestHandshake_ResumeLoadError_FallsBackToNew(t *testing.T) {
	cb := &fakeCallbacks{}
	s, wResp, rReq := newTestSession(t, cb)
	s.acpSessID = ""

	var mu sync.Mutex
	var methods []string
	go mockHandshakeServer(t, rReq, wResp, &mu, &methods, `{}`, true)

	if err := s.handshake("missing-id", ""); err != nil {
		t.Fatalf("handshake should fall back to new instead of erroring, got %v", err)
	}
	if got := s.currentACPSessionID(); got != "brand-new-id" {
		t.Fatalf("acp session id = %q, want brand-new-id from session/new", got)
	}
	if !containsMethod(methods, "session/new") {
		t.Fatalf("expected session/new fallback, methods=%v", methods)
	}
}

// Regression for a handshake self-deadlock. Hermes replays the ENTIRE prior
// conversation as session/update notifications BEFORE it returns the
// session/load response. Before the fix onNotification emitted every replay
// event into the bounded events channel while the engine (which only drains
// Events() after StartSession returns) was still blocked inside handshake;
// once the replay exceeded the channel buffer, readLoop stalled, the load
// response was never read and handshake hung forever (stalling the whole
// engine, so later messages in any chat stopped being processed). The fix
// drops replay updates until the handshake completes. This test hangs/fails
// on the old code and passes after.
func TestHandshake_ResumeLoadReplayDoesNotDeadlock(t *testing.T) {
	cb := &fakeCallbacks{}
	s, wResp, rReq := newTestSession(t, cb) // events channel cap = 32
	s.acpSessID = ""

	const replayN = 80 // deliberately larger than the 32-slot events buffer
	go func() {
		sc := bufio.NewScanner(rReq)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var req map[string]any
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			method, _ := req["method"].(string)
			id := req["id"]
			switch method {
			case "initialize":
				fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%v,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true}}}`+"\n", id)
			case "session/load":
				// Flood replay updates BEFORE the load response, like Hermes does.
				for i := 0; i < replayN; i++ {
					fmt.Fprintf(wResp, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"restored","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replay-%d"}}}}`+"\n", i)
				}
				fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%v,"result":{"modes":{"currentModeId":"default","availableModes":[]}}}`+"\n", id)
			case "session/new":
				fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%v,"result":{"sessionId":"brand-new-id"}}`+"\n", id)
			default:
				fmt.Fprintf(wResp, `{"jsonrpc":"2.0","id":%v,"result":{}}`+"\n", id)
			}
		}
	}()

	done := make(chan error, 1)
	go func() { done <- s.handshake("restored", "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handshake resume: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake deadlocked: replay overflowed the events channel and the session/load response was never consumed")
	}

	if got := s.currentACPSessionID(); got != "restored" {
		t.Fatalf("acp session id = %q, want restored", got)
	}

	// Replay must not have leaked into the live event stream.
	select {
	case ev := <-s.events:
		t.Fatalf("history replay leaked a live event during handshake: %+v", ev)
	default:
	}

	// Once the handshake is done, live updates flow normally again.
	s.onNotification("session/update", json.RawMessage(
		`{"sessionId":"restored","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"live"}}}`))
	select {
	case ev := <-s.events:
		if ev.Content != "live" {
			t.Fatalf("post-handshake live event content = %q, want live", ev.Content)
		}
	case <-time.After(time.Second):
		t.Fatal("live session/update after handshake must be emitted")
	}
}
