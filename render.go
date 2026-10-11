package claudecli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	gotato "github.com/jinhuang712/gotato"
)

// hashAll returns one hash per message over what the model sees, so
// metadata and IDs gotato adds do not break the comparison.
func hashAll(msgs []gotato.Message) []string {
	clean := gotato.ForModel(msgs)
	out := make([]string, len(clean))
	for i, m := range clean {
		raw, _ := json.Marshal(m)
		sum := sha256.Sum256(raw)
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

// toolsSig identifies a tool list. A session serves one tool list.
func toolsSig(tools []gotato.ToolSpec) string {
	h := sha256.New()
	for _, t := range tools {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00", t.ID, t.Description, canonical(t.InputSchema)) // a hash write never fails
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonical re-encodes JSON with sorted keys.
func canonical(raw []byte) string {
	var v any
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// toolName is a tool's name inside the CLI: the ID when it is a plain name,
// else a short hash.
func toolName(id string) string {
	plain := id != "" && len(id) <= 48
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			plain = false
		}
	}
	if plain {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "t_" + hex.EncodeToString(sum[:8])
}

// render writes messages as one prompt. New user messages pass through as
// their text. A new session that needs earlier messages gets them written
// out, then the new message.
func render(msgs []gotato.Message, replay bool) string {
	userOnly := true
	for _, m := range msgs {
		if m.Role != gotato.RoleUser {
			userOnly = false
		}
	}
	if userOnly && (!replay || len(msgs) <= 1) {
		var parts []string
		for _, m := range msgs {
			if t := strings.TrimSpace(gotato.TextOf(m)); t != "" {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	var b strings.Builder
	if replay {
		b.WriteString("The conversation so far follows, written out because this session restarted. Continue from its end.\n\n")
	}
	for _, m := range msgs {
		switch m.Role {
		case gotato.RoleUser:
			b.WriteString(strings.TrimSpace(gotato.TextOf(m)))
		case gotato.RoleAssistant:
			if t := strings.TrimSpace(gotato.TextOf(m)); t != "" {
				b.WriteString("[your reply] " + t)
			}
			for _, c := range m.ToolCalls {
				fmt.Fprintf(&b, "\n[you called %s with %s, call %s]", c.ToolID, canonical(c.Arguments), c.ID)
			}
		case gotato.RoleToolResult:
			if m.ToolResult != nil {
				text, isError := resultText(*m.ToolResult)
				status := "result"
				if isError {
					status = "error"
				}
				fmt.Fprintf(&b, "[%s of call %s] %s", status, m.ToolResult.CallID, text)
			}
		default:
			b.WriteString(strings.TrimSpace(gotato.TextOf(m)))
		}
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// resultText is a tool result as text, and whether it is an error.
func resultText(r gotato.ToolResult) (string, bool) {
	var parts []string
	for _, p := range r.Content {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	text := strings.Join(parts, "\n")
	isError := r.Status != gotato.ToolResultOK
	if isError && r.SafeError != "" {
		if text != "" {
			text += "\n"
		}
		text += r.SafeError
	}
	if text == "" {
		text = string(r.Status)
	}
	return text, isError
}
