package claudecli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	gotato "github.com/jinhuang712/gotato"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// response is one scripted model response: text that ends the turn, or tool
// calls.
type response struct {
	text  string
	calls []gotato.ToolCall
	hang  bool // never finishes
}

// fakeCLI plays a script the way the CLI does: it streams a response, then
// calls the tool handlers one at a time and waits for their results before
// the next response.
type fakeCLI struct {
	mu      sync.Mutex
	script  []response
	dials   []spec
	prompts []string
	results []string
	closed  int
}

func (f *fakeCLI) dial(_ context.Context, sp spec) (conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials = append(f.dials, sp)
	id := sp.Resume
	if id == "" {
		id = fmt.Sprintf("sess-%d", len(f.dials))
	}
	return &fakeConn{f: f, sp: sp, id: id, msgs: make(chan claudecode.Message, 64), quit: make(chan struct{})}, nil
}

func (f *fakeCLI) next() (response, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.script) == 0 {
		return response{}, false
	}
	r := f.script[0]
	f.script = f.script[1:]
	return r, true
}

func (f *fakeCLI) snapshot() (dials []spec, prompts, results []string, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]spec(nil), f.dials...), append([]string(nil), f.prompts...), append([]string(nil), f.results...), f.closed
}

type fakeConn struct {
	f    *fakeCLI
	sp   spec
	id   string
	msgs chan claudecode.Message
	quit chan struct{}
	once sync.Once
}

func (c *fakeConn) Messages() <-chan claudecode.Message { return c.msgs }

func (c *fakeConn) Close() error {
	c.once.Do(func() {
		close(c.quit)
		c.f.mu.Lock()
		c.f.closed++
		c.f.mu.Unlock()
	})
	return nil
}

func (c *fakeConn) send(m claudecode.Message) bool {
	select {
	case c.msgs <- m:
		return true
	case <-c.quit:
		return false
	}
}

func (c *fakeConn) event(e map[string]any) bool {
	return c.send(&claudecode.StreamEvent{SessionID: c.id, Event: e})
}

// Query takes the turn's first response at once, so the process that got
// the prompt answers it.
func (c *fakeConn) Query(_ context.Context, prompt string) error {
	c.f.mu.Lock()
	c.f.prompts = append(c.f.prompts, prompt)
	c.f.mu.Unlock()
	first, ok := c.f.next()
	go c.turn(first, ok)
	return nil
}

// turn plays responses until one ends the turn. A closed process plays
// nothing more.
func (c *fakeConn) turn(r response, ok bool) {
	for {
		select {
		case <-c.quit:
			return
		default:
		}
		if !ok || r.hang {
			return
		}
		if !c.event(map[string]any{"type": "message_start"}) {
			return
		}
		if r.text != "" {
			c.event(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": r.text}})
		}
		for _, call := range r.calls {
			var input map[string]any
			_ = json.Unmarshal(call.Arguments, &input)
			c.send(&claudecode.AssistantMessage{Content: []claudecode.ContentBlock{&claudecode.ToolUseBlock{
				ToolUseID: string(call.ID), Name: "mcp__gotato__" + toolName(call.ToolID), Input: input}}})
		}
		stop := "end_turn"
		if len(r.calls) > 0 {
			stop = "tool_use"
		}
		c.event(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop},
			"usage": map[string]any{"input_tokens": 2.0, "cache_read_input_tokens": 100.0, "cache_creation_input_tokens": 50.0, "output_tokens": 7.0}})
		c.event(map[string]any{"type": "message_stop"})
		if len(r.calls) == 0 {
			c.send(&claudecode.ResultMessage{Subtype: "success", SessionID: c.id})
			return
		}
		for _, call := range r.calls {
			var input map[string]any
			_ = json.Unmarshal(call.Arguments, &input)
			text, _ := c.sp.Call(context.Background(), call.ToolID, input)
			if text == "the session closed" {
				return // the process is being closed and plays nothing more
			}
			c.f.mu.Lock()
			c.f.results = append(c.f.results, text)
			c.f.mu.Unlock()
		}
		select {
		case <-c.quit:
			return
		default:
		}
		r, ok = c.f.next()
	}
}

// testClock is a settable clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestPool(t *testing.T, f *fakeCLI, cfg Config) (*Pool, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	p := newPool(cfg, f.dial, clock.now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = p.Close() })
	return p, clock
}

// call runs one Stream to its end and returns its events.
func call(t *testing.T, m gotato.Model, req gotato.ModelRequest) ([]gotato.ModelEvent, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := m.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	var out []gotato.ModelEvent
	for {
		ev, err := st.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
}

func user(text string) gotato.Message { return gotato.UserMessage(text) }

func assistantCalls(calls ...gotato.ToolCall) gotato.Message {
	return gotato.Message{Role: gotato.RoleAssistant, ToolCalls: calls}
}

func result(id, text string) gotato.Message {
	return gotato.Message{Role: gotato.RoleToolResult, ToolResult: &gotato.ToolResult{CallID: gotato.ToolCallID(id),
		Status: gotato.ToolResultOK, Content: []gotato.ContentPart{{Kind: gotato.ContentText, Text: text}}}}
}

func toolCall(id, tool, args string) gotato.ToolCall {
	return gotato.ToolCall{ID: gotato.ToolCallID(id), ToolID: tool, Arguments: []byte(args)}
}

var tools = []gotato.ToolSpec{
	{ID: "read_file", Description: "read", InputSchema: []byte(`{"type":"object"}`)},
	{ID: "spawn_worker", Description: "spawn", InputSchema: []byte(`{"type":"object"}`)},
}

func kinds(events []gotato.ModelEvent) string {
	var out []string
	for _, ev := range events {
		s := string(ev.Kind)
		switch ev.Kind {
		case gotato.ModelTextDelta:
			s += "(" + ev.Text + ")"
		case gotato.ModelToolCall:
			s += "(" + string(ev.ToolCall.ID) + " " + ev.ToolCall.ToolID + ")"
		case gotato.ModelDone:
			s += "(" + string(ev.StopReason) + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestATextTurnStreamsItsReplyAndUsage(t *testing.T) {
	f := &fakeCLI{script: []response{{text: "hello"}}}
	p, _ := newTestPool(t, f, Config{})
	events, err := call(t, p.Model("a.e1", "m"), gotato.ModelRequest{SystemInstructions: "sys", Messages: []gotato.Message{user("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(events); got != "text_delta(hello) usage done(end_turn)" {
		t.Fatalf("events = %s", got)
	}
	if u := events[1].Usage; u.InputTokens != 152 || u.CacheReadTokens != 100 || u.CacheWriteTokens != 50 || u.OutputTokens != 7 {
		t.Fatalf("usage = %+v, want cache reads and writes counted as input", u)
	}
	dials, prompts, _, _ := f.snapshot()
	if len(dials) != 1 || dials[0].System != "sys" || dials[0].Model != "m" || strings.Join(prompts, "|") != "hi" {
		t.Fatalf("dials %+v, prompts %q", dials, prompts)
	}
}

func TestToolRoundsSendOnlyTheResults(t *testing.T) {
	a, b := toolCall("c1", "read_file", `{"path":"a"}`), toolCall("c2", "read_file", `{"path":"b"}`)
	f := &fakeCLI{script: []response{{calls: []gotato.ToolCall{a, b}}, {text: "done"}}}
	p, _ := newTestPool(t, f, Config{})
	m := p.Model("a.brief-1", "m")
	first := []gotato.Message{user("go")}
	events, err := call(t, m, gotato.ModelRequest{Tools: tools, Messages: first})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(events); got != "tool_call(c1 read_file) tool_call(c2 read_file) usage done(tool_calls)" {
		t.Fatalf("first call = %s", got)
	}
	second := append(first, assistantCalls(a, b), result("c1", "A"), result("c2", "B"))
	events, err = call(t, m, gotato.ModelRequest{Tools: tools, Messages: second})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(events); got != "text_delta(done) usage done(end_turn)" {
		t.Fatalf("second call = %s", got)
	}
	dials, prompts, results, _ := f.snapshot()
	if len(dials) != 1 || len(prompts) != 1 || strings.Join(results, "|") != "A|B" {
		t.Fatalf("dials %d, prompts %q, results %q; want one process, one prompt, and the results in order", len(dials), prompts, results)
	}
}

func TestALaterRequestReachesTheModelWithTheLastResult(t *testing.T) {
	r, s := toolCall("c1", "read_file", `{"path":"x"}`), toolCall("c2", "spawn_worker", `{"goal":"g"}`)
	f := &fakeCLI{script: []response{{calls: []gotato.ToolCall{r, s}}, {text: "ok"}}}
	p, _ := newTestPool(t, f, Config{})
	m := p.Model("a.e1", "m")
	batch1 := []gotato.Message{user("batch one")}
	if _, err := call(t, m, gotato.ModelRequest{Tools: tools, Messages: batch1}); err != nil {
		t.Fatal(err)
	}
	// The request stopped after its actions. Their results come with the
	// next request.
	next := append(batch1, assistantCalls(r, s), result("c1", "read"), result("c2", "queued #1"), user("batch two"))
	events, err := call(t, m, gotato.ModelRequest{Tools: tools, Messages: next})
	if err != nil {
		t.Fatal(err)
	}
	_, prompts, results, _ := f.snapshot()
	if kinds(events) != "text_delta(ok) usage done(end_turn)" || len(prompts) != 1 ||
		strings.Join(results, "|") != "read|queued #1\n\nbatch two" {
		t.Fatalf("events %s, prompts %q, results %q", kinds(events), prompts, results)
	}
}

func TestADifferentHistoryStartsANewSessionWithTheConversation(t *testing.T) {
	f := &fakeCLI{script: []response{{text: "one"}, {text: "two"}}}
	p, _ := newTestPool(t, f, Config{})
	m := p.Model("a.e1", "m")
	if _, err := call(t, m, gotato.ModelRequest{Messages: []gotato.Message{user("first")}}); err != nil {
		t.Fatal(err)
	}
	rewritten := []gotato.Message{user("first"), gotato.AssistantMessage("not what you said"), user("second")}
	if _, err := call(t, m, gotato.ModelRequest{Messages: rewritten}); err != nil {
		t.Fatal(err)
	}
	dials, prompts, _, closed := f.snapshot()
	if len(dials) != 2 || closed != 1 || dials[1].Resume != "" {
		t.Fatalf("dials %+v, closed %d; want the old process closed and a new session", dials, closed)
	}
	if !strings.Contains(prompts[1], "restarted") || !strings.Contains(prompts[1], "[your reply] not what you said") ||
		!strings.HasSuffix(prompts[1], "second") {
		t.Fatalf("replay prompt = %q", prompts[1])
	}
}

func TestAnIdleSessionClosesAndResumesByItsID(t *testing.T) {
	f := &fakeCLI{script: []response{{text: "one"}, {text: "two"}}}
	p, now := newTestPool(t, f, Config{IdleClose: time.Minute})
	m := p.Model("a.e1", "m")
	first := []gotato.Message{user("first")}
	if _, err := call(t, m, gotato.ModelRequest{Messages: first}); err != nil {
		t.Fatal(err)
	}
	now.add(2 * time.Minute)
	p.closeIdle()
	if p.Live() != 0 {
		t.Fatal("an idle session must close its process")
	}
	next := append(first, gotato.AssistantMessage("one"), user("second"))
	if _, err := call(t, m, gotato.ModelRequest{Messages: next}); err != nil {
		t.Fatal(err)
	}
	dials, prompts, _, _ := f.snapshot()
	if len(dials) != 2 || dials[1].Resume != "sess-1" || prompts[1] != "second" {
		t.Fatalf("dials %+v, prompts %q; want a resume by ID that sends only the new message", dials, prompts)
	}
}

func TestResultsLostWithAClosedProcessArriveAsText(t *testing.T) {
	c := toolCall("c1", "spawn_worker", `{"goal":"g"}`)
	f := &fakeCLI{script: []response{{calls: []gotato.ToolCall{c}}, {text: "ok"}}}
	p, now := newTestPool(t, f, Config{IdleClose: time.Minute})
	m := p.Model("a.e1", "m")
	batch1 := []gotato.Message{user("batch one")}
	if _, err := call(t, m, gotato.ModelRequest{Tools: tools, Messages: batch1}); err != nil {
		t.Fatal(err)
	}
	now.add(2 * time.Minute)
	p.closeIdle()
	next := append(batch1, assistantCalls(c), result("c1", "queued #1"), user("batch two"))
	if _, err := call(t, m, gotato.ModelRequest{Tools: tools, Messages: next}); err != nil {
		t.Fatal(err)
	}
	dials, prompts, _, _ := f.snapshot()
	if len(dials) != 2 || dials[1].Resume != "sess-1" || !strings.Contains(prompts[1], "[result of call c1] queued #1") ||
		!strings.HasSuffix(prompts[1], "batch two") {
		t.Fatalf("dials %+v, prompts %q", dials, prompts)
	}
}

func TestACancelledCallStartsOverNextTime(t *testing.T) {
	f := &fakeCLI{script: []response{{hang: true}, {text: "fresh"}}}
	p, _ := newTestPool(t, f, Config{})
	m := p.Model("a.e1", "m")
	ctx, cancel := context.WithCancel(context.Background())
	st, err := m.Stream(ctx, gotato.ModelRequest{Messages: []gotato.Message{user("first")}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := st.Recv(context.Background()); err == nil {
		t.Fatal("a cancelled call must fail")
	}
	_ = st.Close()
	if _, err := call(t, m, gotato.ModelRequest{Messages: []gotato.Message{user("first"), gotato.AssistantMessage(""), user("again")}}); err != nil {
		t.Fatal(err)
	}
	dials, _, _, closed := f.snapshot()
	if len(dials) != 2 || dials[1].Resume != "" || closed < 1 {
		t.Fatalf("dials %+v, closed %d; want the process closed and a new session", dials, closed)
	}
}

func TestMaxSessionsClosesTheLeastRecentlyUsed(t *testing.T) {
	f := &fakeCLI{script: []response{{text: "a"}, {text: "b"}}}
	p, now := newTestPool(t, f, Config{MaxSessions: 1})
	if _, err := call(t, p.Model("a", "m"), gotato.ModelRequest{Messages: []gotato.Message{user("x")}}); err != nil {
		t.Fatal(err)
	}
	now.add(time.Second)
	if _, err := call(t, p.Model("b", "m"), gotato.ModelRequest{Messages: []gotato.Message{user("y")}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, closed := f.snapshot(); closed != 1 || p.Live() != 1 {
		t.Fatalf("closed %d, live %d; want the older session closed", closed, p.Live())
	}
}
