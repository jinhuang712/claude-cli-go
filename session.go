package claudecli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gotato "github.com/jinhuang712/gotato"
	claudecode "github.com/severity1/claude-agent-sdk-go"
)

// session is one conversation. mu is held for the whole of one call.
type session struct {
	key  string
	pool *Pool

	mu   sync.Mutex
	conn conn
	gen  chan struct{} // closed when conn closes; tool handlers watch it
	live atomic.Bool
	id   string // the CLI's session ID, for resume

	model, system, tools string
	names                map[string]string // in-process tool name → tool ID
	seen                 []string          // hashes of the request messages the CLI holds
	last                 reply             // the response the last call returned

	used atomic.Int64 // unix nanoseconds of the last call's end

	pmu     sync.Mutex
	pending []*pendingCall // the tool calls the CLI waits on, in order
	added   chan struct{}  // closed and replaced when a call is added
}

// reply identifies a response, so the next request can be checked against
// it: its tool call IDs, or its text when it called no tool.
type reply struct {
	calls []string
	text  string
	set   bool
}

func (r reply) matches(m gotato.Message) bool {
	if !r.set || m.Role != gotato.RoleAssistant || len(m.ToolCalls) != len(r.calls) {
		return false
	}
	for i, c := range m.ToolCalls {
		if string(c.ID) != r.calls[i] {
			return false
		}
	}
	return len(r.calls) > 0 || strings.TrimSpace(gotato.TextOf(m)) == strings.TrimSpace(r.text)
}

// pendingCall is a tool call of the last response. Its result can arrive
// before the CLI calls its handler, since the CLI runs the calls one at a
// time.
type pendingCall struct {
	id        string
	tool      string
	args      string // canonical JSON
	claimed   bool
	delivered bool
	result    chan toolResult
}

type toolResult struct {
	text    string
	isError bool
}

func (s *session) usedAt() time.Time { return time.Unix(0, s.used.Load()) }

func (s *session) release() {
	s.used.Store(s.pool.now().UnixNano())
	s.mu.Unlock()
}

// begin brings the CLI to the request: it sends what is new to a live
// session, resumes a closed one, or starts a new one.
func (s *session) begin(ctx context.Context, modelID string, req gotato.ModelRequest) error {
	hashes := hashAll(req.Messages)
	sig := toolsSig(req.Tools)
	delta, ok := s.extends(modelID, req, hashes, sig)
	if ok && s.conn == nil && s.id != "" {
		if err := s.start(ctx, modelID, req, s.id); err != nil {
			s.pool.logger.Warn("resume a CLI session", "key", s.key, "err", err)
			ok = false
		} else {
			// The results the closed process waited on reach the model as text.
			s.dropPending()
		}
	}
	if ok && s.conn != nil {
		s.seen = hashes
		if s.hasPending() {
			err := s.deliver(delta)
			if err == nil {
				return nil
			}
			s.pool.logger.Debug("deliver tool results", "key", s.key, "err", err)
		} else if prompt := render(delta, false); prompt != "" {
			return s.conn.Query(ctx, prompt)
		}
	}
	s.pool.logger.Debug("start a new CLI session", "key", s.key, "continues", ok, "live", s.conn != nil,
		"held", len(s.seen), "messages", len(req.Messages))
	s.closeConn()
	s.forget()
	if err := s.start(ctx, modelID, req, ""); err != nil {
		return err
	}
	s.model, s.system, s.tools, s.seen = modelID, req.SystemInstructions, sig, hashes
	prompt := render(req.Messages, true)
	if prompt == "" {
		return errorf("the request has nothing to send")
	}
	return s.conn.Query(ctx, prompt)
}

// extends returns the new messages when req continues the session: the
// messages it holds, then the response it gave last, then new messages.
func (s *session) extends(modelID string, req gotato.ModelRequest, hashes []string, sig string) ([]gotato.Message, bool) {
	n := len(s.seen)
	if n == 0 || s.model != modelID || s.system != req.SystemInstructions || s.tools != sig || len(hashes) <= n {
		return nil, false
	}
	for i := range n {
		if hashes[i] != s.seen[i] {
			return nil, false
		}
	}
	if !s.last.matches(req.Messages[n]) {
		return nil, false
	}
	return req.Messages[n+1:], true
}

// start runs a CLI process, resuming the session ID when given.
func (s *session) start(ctx context.Context, modelID string, req gotato.ModelRequest, resume string) error {
	s.pool.makeRoom(s)
	s.names = map[string]string{}
	for _, t := range req.Tools {
		s.names[toolName(t.ID)] = t.ID
	}
	gen := make(chan struct{})
	c, err := s.pool.dial(ctx, spec{Model: modelID, System: req.SystemInstructions, Tools: req.Tools, Resume: resume,
		Call: func(ctx context.Context, toolID string, args map[string]any) (string, bool) {
			return s.handle(ctx, gen, toolID, args)
		}})
	if err != nil {
		return errorf("start the CLI: %w", err)
	}
	s.conn, s.gen = c, gen
	s.live.Store(true)
	return nil
}

// closeConn ends the process in the background: a CLI can take seconds to
// exit. The session keeps its ID and history, so its next call resumes.
func (s *session) closeConn() {
	if s.conn == nil {
		return
	}
	close(s.gen)
	c := s.conn
	s.pool.closing.Add(1)
	go func() {
		defer s.pool.closing.Done()
		_ = c.Close()
	}()
	s.conn = nil
	s.live.Store(false)
}

// forget drops the session's history, so its next call starts a new one.
func (s *session) forget() {
	s.id, s.seen, s.last = "", nil, reply{}
	s.dropPending()
}

// hasPending reports whether the CLI waits on tool calls whose results have
// not been delivered.
func (s *session) hasPending() bool {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	for _, p := range s.pending {
		if !p.delivered {
			return true
		}
	}
	return false
}

// newResponse drops the calls of the previous response: the CLI has run
// all of them before it asks for the next response.
func (s *session) newResponse() {
	s.pmu.Lock()
	s.pending = nil
	s.pmu.Unlock()
}

func (s *session) dropPending() {
	s.pmu.Lock()
	s.pending = nil
	s.pmu.Unlock()
}

// addPending records a tool call of the current response and wakes the
// handlers waiting to claim one.
func (s *session) addPending(id, tool string, args []byte) {
	s.pmu.Lock()
	s.pending = append(s.pending, &pendingCall{id: id, tool: tool, args: canonical(args), result: make(chan toolResult, 1)})
	if s.added != nil {
		close(s.added)
	}
	s.added = make(chan struct{})
	s.pmu.Unlock()
}

// handle serves one tool call from the CLI: it claims the matching call of
// the current response, then waits for the caller's result.
func (s *session) handle(ctx context.Context, gen chan struct{}, toolID string, args map[string]any) (string, bool) {
	raw, _ := json.Marshal(args)
	want := canonical(raw)
	deadline := time.NewTimer(claimWait)
	defer deadline.Stop()
	var call *pendingCall
	for call == nil {
		s.pmu.Lock()
		for _, p := range s.pending {
			if !p.claimed && p.tool == toolID && p.args == want {
				p.claimed = true
				call = p
				break
			}
		}
		if s.added == nil {
			s.added = make(chan struct{})
		}
		added := s.added
		s.pmu.Unlock()
		if call != nil {
			break
		}
		select {
		case <-added:
		case <-deadline.C:
			return "no matching tool call in the response", true
		case <-ctx.Done():
			return "cancelled", true
		case <-gen:
			return "the session closed", true
		}
	}
	select {
	case r := <-call.result:
		return r.text, r.isError
	case <-ctx.Done():
		return "cancelled", true
	case <-gen:
		return "the session closed", true
	}
}

// deliver hands the waiting tool calls their results. Messages after the
// results reach the model with the last result, since the CLI continues as
// soon as every call has its result.
func (s *session) deliver(delta []gotato.Message) error {
	results := map[string]gotato.ToolResult{}
	i := 0
	for ; i < len(delta) && delta[i].Role == gotato.RoleToolResult && delta[i].ToolResult != nil; i++ {
		results[string(delta[i].ToolResult.CallID)] = *delta[i].ToolResult
	}
	extra := render(delta[i:], false)
	s.pmu.Lock()
	defer s.pmu.Unlock()
	var waiting []*pendingCall
	for _, p := range s.pending {
		if !p.delivered {
			waiting = append(waiting, p)
		}
	}
	if len(results) != len(waiting) {
		return errMismatch
	}
	for _, p := range waiting {
		if _, ok := results[p.id]; !ok {
			return errMismatch
		}
	}
	for j, p := range waiting {
		text, isError := resultText(results[p.id])
		if j == len(waiting)-1 && extra != "" {
			text += "\n\n" + extra
		}
		p.delivered = true
		p.result <- toolResult{text: text, isError: isError}
	}
	return nil
}

// read streams one model response: text and reasoning as they arrive, each
// tool call, the usage, and the stop. A response that ends the turn also
// waits for the turn's result.
func (s *session) read(ctx context.Context, out chan<- gotato.ModelEvent) error {
	emit := func(ev gotato.ModelEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	msgs := s.conn.Messages()
	var (
		calls    []string
		text     strings.Builder
		stop     string
		usage    *gotato.Usage
		streamed bool // text arrived as deltas
	)
	for {
		var m claudecode.Message
		select {
		case <-ctx.Done():
			return ctx.Err()
		case next, ok := <-msgs:
			if !ok {
				s.closeConn()
				s.forget()
				return errorf("the CLI session ended")
			}
			m = next
		}
		switch msg := m.(type) {
		case *claudecode.SystemMessage:
			if id, _ := msg.Data["session_id"].(string); id != "" {
				s.id = id
			}
		case *claudecode.StreamEvent:
			if msg.SessionID != "" {
				s.id = msg.SessionID
			}
			if msg.ParentToolUseID != nil {
				continue
			}
			switch kind, _ := msg.Event["type"].(string); kind {
			case "message_start":
				calls, stop, usage, streamed = nil, "", nil, false
				text.Reset()
				s.newResponse()
			case "content_block_delta":
				d, _ := msg.Event["delta"].(map[string]any)
				switch t, _ := d["type"].(string); t {
				case "text_delta":
					chunk, _ := d["text"].(string)
					text.WriteString(chunk)
					streamed = true
					if !emit(gotato.ModelEvent{Kind: gotato.ModelTextDelta, Text: chunk}) {
						return ctx.Err()
					}
				case "thinking_delta":
					chunk, _ := d["thinking"].(string)
					if !emit(gotato.ModelEvent{Kind: gotato.ModelReasoningDelta, Text: chunk}) {
						return ctx.Err()
					}
				}
			case "message_delta":
				if d, ok := msg.Event["delta"].(map[string]any); ok {
					stop, _ = d["stop_reason"].(string)
				}
				if u, ok := msg.Event["usage"].(map[string]any); ok {
					v := usageOf(u)
					usage = &v
				}
			case "message_stop":
				if usage != nil && !emit(gotato.ModelEvent{Kind: gotato.ModelUsage, Usage: *usage}) {
					return ctx.Err()
				}
				if stop == "tool_use" {
					s.last = reply{calls: calls, set: true}
					if !emit(gotato.ModelEvent{Kind: gotato.ModelDone, StopReason: gotato.StopToolCalls}) {
						return ctx.Err()
					}
					return nil
				}
				s.last = reply{text: text.String(), set: true}
				if !emit(gotato.ModelEvent{Kind: gotato.ModelDone, StopReason: stopReason(stop)}) {
					return ctx.Err()
				}
				return s.awaitResult(ctx)
			}
		case *claudecode.AssistantMessage:
			if msg.ParentToolUseID != nil {
				continue
			}
			for _, b := range msg.Content {
				switch blk := b.(type) {
				case *claudecode.ToolUseBlock:
					args, _ := json.Marshal(blk.Input)
					toolID := s.toolID(blk.Name)
					s.addPending(blk.ToolUseID, toolID, args)
					calls = append(calls, blk.ToolUseID)
					if !emit(gotato.ModelEvent{Kind: gotato.ModelToolCall, ToolCall: &gotato.ToolCall{
						ID: gotato.ToolCallID(blk.ToolUseID), ToolID: toolID, Arguments: args}}) {
						return ctx.Err()
					}
				case *claudecode.TextBlock:
					if !streamed {
						text.WriteString(blk.Text)
						if !emit(gotato.ModelEvent{Kind: gotato.ModelTextDelta, Text: blk.Text}) {
							return ctx.Err()
						}
					}
				case *claudecode.ThinkingBlock:
					artifact, _ := json.Marshal(map[string]string{"type": "thinking", "thinking": blk.Thinking, "signature": blk.Signature})
					if !emit(gotato.ModelEvent{Kind: gotato.ModelReasoningDone, ReasoningArtifact: artifact}) {
						return ctx.Err()
					}
				}
			}
		case *claudecode.ResultMessage:
			if msg.SessionID != "" {
				s.id = msg.SessionID
			}
			// A turn that ends before a response's stop event failed or
			// was interrupted.
			return errorf("the turn ended early: %s", msg.Subtype)
		}
	}
}

// awaitResult reads to the end of the turn, so the next call starts clean.
func (s *session) awaitResult(ctx context.Context) error {
	msgs := s.conn.Messages()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-msgs:
			if !ok {
				s.closeConn()
				return nil
			}
			if r, ok := m.(*claudecode.ResultMessage); ok {
				if r.SessionID != "" {
					s.id = r.SessionID
				}
				return nil
			}
		}
	}
}

// toolID maps the CLI's name for an in-process tool back to the tool ID.
func (s *session) toolID(name string) string {
	short := strings.TrimPrefix(name, "mcp__"+s.pool.cfg.ServerName+"__")
	if id, ok := s.names[short]; ok {
		return id
	}
	return short
}

func stopReason(s string) gotato.StopReason {
	switch s {
	case "max_tokens":
		return gotato.StopMaxTokens
	case "tool_use":
		return gotato.StopToolCalls
	default:
		return gotato.StopEndTurn
	}
}

// usageOf reads a response's usage. gotato counts cache reads and writes as
// input, like its Messages adapter.
func usageOf(u map[string]any) gotato.Usage {
	n := func(k string) uint64 {
		f, _ := u[k].(float64)
		return uint64(f)
	}
	input := n("input_tokens") + n("cache_read_input_tokens") + n("cache_creation_input_tokens")
	return gotato.Usage{InputTokens: input, OutputTokens: n("output_tokens"), TotalTokens: input + n("output_tokens"),
		CacheReadTokens: n("cache_read_input_tokens"), CacheWriteTokens: n("cache_creation_input_tokens")}
}
