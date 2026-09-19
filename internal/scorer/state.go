package scorer

import (
	"strings"

	"jev-proxy/internal/config"
	"jev-proxy/internal/jev"
)

// BuildState assembles the judge input from the chat request and the buffered
// reply, following the doc's truncation rules: system excerpt, the last N
// messages of the conversation, the final user message, and the full reply.
// Truncation keeps the tail (the newest content) and marks the cut.
func BuildState(msgs []ChatMessage, reply string, sc *config.Scoring) jev.State {
	systems := make([]string, 0, 2)
	rest := make([]ChatMessage, 0, len(msgs))
	var lastUser string
	for _, m := range msgs {
		switch {
		case m.Role == "system":
			systems = append(systems, m.Text())
		case m.Role == "user":
			rest = append(rest, m)
			lastUser = m.Text()
		default:
			rest = append(rest, m)
		}
	}

	keep := len(rest)
	if keep > sc.ContextTurns {
		keep = sc.ContextTurns
	}
	tail := rest[len(rest)-keep:]
	var b strings.Builder
	for _, m := range tail {
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(truncateTail(m.Text(), sc.MessageMaxChars))
		b.WriteString("\n\n")
	}

	return jev.State{
		SystemPromptExcerpt: truncateTail(strings.Join(systems, "\n\n"), sc.SystemMaxChars),
		Conversation:        b.String(),
		UserLastMessage:     truncateTail(lastUser, sc.MessageMaxChars),
		AssistantReply:      truncateTail(reply, sc.ReplyMaxChars),
	}
}

// truncateTail keeps the last max runes of s, prefixing an ellipsis marker
// when content was dropped. Empty input stays empty.
func truncateTail(s string, max int) string {
	r := []rune(s)
	if max <= 0 || len(r) <= max {
		return s
	}
	return "…(truncated)" + string(r[len(r)-max:])
}
