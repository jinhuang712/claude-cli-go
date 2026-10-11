// Package claudecli serves gotato.Model calls from long-lived Claude Code
// CLI sessions, one per conversation key. The caller sees a gotato.Model and
// nothing else, so an agent built on gotato runs the same on this adapter
// as on a Messages API endpoint.
//
// A conversation's requests extend each other, so a live session receives
// only what is new: a user message, or the results of the tool calls it
// waits on. The CLI runs the request's tools as in-process tools whose
// handlers wait until the next request brings the caller's results. Each
// call ends at the end of one model response, on the response's stop event,
// with that response's tool calls and usage.
//
// A session whose process closed resumes by its ID. A request that does not
// extend the session starts a new one, seeded with the conversation as text.
package claudecli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	gotato "github.com/jinhuang712/gotato"
)

// Config tunes a Pool.
type Config struct {
	// CLIPath is the CLI binary. Empty finds it on PATH.
	CLIPath string
	// Dir is the CLI processes' working directory.
	Dir string
	// IdleClose ends a session's process after this long without a call.
	// The session resumes by its ID on its next call.
	IdleClose time.Duration
	// MaxSessions bounds live processes. Past it, the least recently used
	// idle session closes first.
	MaxSessions int
	// ServerName names the in-process tool server. The model sees it in
	// tool names, as mcp__<server>__<tool>. Empty means "gotato".
	ServerName string
}

// claimWait bounds how long a tool handler waits for its call to appear in
// the response stream.
const claimWait = 10 * time.Second

// errMismatch reports a request that does not continue the session.
var errMismatch = errors.New("claudecli: the request does not continue the session")

// Pool owns the sessions.
type Pool struct {
	cfg    Config
	dial   dialer
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	stop     chan struct{}
	wg       sync.WaitGroup
	closing  sync.WaitGroup // processes being closed
}

// New starts a pool on the CLI's SDK.
func New(cfg Config, logger *slog.Logger) *Pool {
	cfg = cfg.withDefaults()
	return newPool(cfg, sdkDialer(cfg), time.Now, logger)
}

func (c Config) withDefaults() Config {
	if c.IdleClose <= 0 {
		c.IdleClose = 10 * time.Minute
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 8
	}
	if c.ServerName == "" {
		c.ServerName = "gotato"
	}
	return c
}

func newPool(cfg Config, dial dialer, now func() time.Time, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.withDefaults()
	p := &Pool{cfg: cfg, dial: dial, now: now, logger: logger, sessions: map[string]*session{}, stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.sweep()
	}()
	return p
}

// Model returns a model for the conversation key on the model ID.
func (p *Pool) Model(key, modelID string) gotato.Model { return &model{pool: p, key: key, id: modelID} }

// Close ends every session's process, all at once, and waits for them.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.stop)
	all := make([]*session, 0, len(p.sessions))
	for _, s := range p.sessions {
		all = append(all, s)
	}
	p.mu.Unlock()
	p.wg.Wait()
	for _, s := range all {
		s.mu.Lock()
		s.closeConn()
		s.mu.Unlock()
	}
	p.closing.Wait()
	return nil
}

// Live is the number of sessions with a running process.
func (p *Pool) Live() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.sessions {
		if s.live.Load() {
			n++
		}
	}
	return n
}

// acquire returns the key's session, locked for one call.
func (p *Pool) acquire(key string) (*session, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("claudecli: the pool is closed")
	}
	s, ok := p.sessions[key]
	if !ok {
		s = &session{key: key, pool: p, names: map[string]string{}}
		p.sessions[key] = s
	}
	p.mu.Unlock()
	s.mu.Lock()
	return s, nil
}

// makeRoom closes the least recently used idle session while the pool is
// at its bound. keep is the session about to start.
func (p *Pool) makeRoom(keep *session) {
	p.mu.Lock()
	var live []*session
	for _, s := range p.sessions {
		if s != keep && s.live.Load() {
			live = append(live, s)
		}
	}
	p.mu.Unlock()
	for len(live)+1 > p.cfg.MaxSessions {
		oldest := -1
		for i, s := range live {
			if oldest < 0 || s.usedAt().Before(live[oldest].usedAt()) {
				oldest = i
			}
		}
		s := live[oldest]
		live = append(live[:oldest], live[oldest+1:]...)
		if s.mu.TryLock() {
			s.closeConn()
			s.mu.Unlock()
		}
	}
}

// sweep closes sessions idle past IdleClose.
func (p *Pool) sweep() {
	t := time.NewTicker(max(p.cfg.IdleClose/4, time.Second))
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
		p.closeIdle()
	}
}

// closeIdle closes every session with no call for IdleClose.
func (p *Pool) closeIdle() {
	p.mu.Lock()
	all := make([]*session, 0, len(p.sessions))
	for _, s := range p.sessions {
		all = append(all, s)
	}
	p.mu.Unlock()
	for _, s := range all {
		if !s.live.Load() || p.now().Sub(s.usedAt()) < p.cfg.IdleClose {
			continue
		}
		if s.mu.TryLock() {
			s.closeConn()
			s.mu.Unlock()
		}
	}
}

type model struct {
	pool *Pool
	key  string
	id   string
}

// Stream sends what the request adds to the key's session and streams the
// next model response.
func (m *model) Stream(ctx context.Context, req gotato.ModelRequest) (gotato.ModelStream, error) {
	s, err := m.pool.acquire(m.key)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithCancel(ctx)
	if err := s.begin(rctx, m.id, req); err != nil {
		cancel()
		s.release()
		return nil, err
	}
	st := &stream{events: make(chan gotato.ModelEvent, 64), cancel: cancel}
	go func() {
		defer s.release()
		defer close(st.events)
		err := s.read(rctx, st.events)
		if err != nil && rctx.Err() != nil {
			// The caller stopped mid-response: the session's state is
			// unknown, so the next call starts over.
			s.closeConn()
			s.forget()
		}
		st.setErr(err)
		cancel()
	}()
	return st, nil
}

// stream hands one response's events to the caller.
type stream struct {
	events chan gotato.ModelEvent
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

func (st *stream) setErr(err error) {
	st.mu.Lock()
	st.err = err
	st.mu.Unlock()
}

func (st *stream) Recv(ctx context.Context) (gotato.ModelEvent, error) {
	select {
	case ev, ok := <-st.events:
		if ok {
			return ev, nil
		}
		st.mu.Lock()
		err := st.err
		st.mu.Unlock()
		if err != nil {
			return gotato.ModelEvent{}, err
		}
		return gotato.ModelEvent{}, io.EOF
	case <-ctx.Done():
		return gotato.ModelEvent{}, ctx.Err()
	}
}

// Close stops the response if it is still running and waits for it.
func (st *stream) Close() error {
	st.cancel()
	for range st.events {
	}
	return nil
}

func errorf(format string, args ...any) error { return fmt.Errorf("claudecli: "+format, args...) }
