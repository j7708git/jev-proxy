package scorer

import (
	"strings"
	"testing"
	"unicode/utf8"

	"jev-proxy/internal/config"
)

func testScoring() *config.Scoring {
	return &config.Scoring{
		ContextTurns:    2,
		SystemMaxChars:  20,
		MessageMaxChars: 10,
		ReplyMaxChars:   5,
	}
}

func raw(s string) []byte { return []byte(s) }

const truncMark = "…(truncated)"

func TestBuildStateTruncationAndTurns(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: raw(`"你是一個開發助理，負責回答 Go 與系統相關問題，請用繁體中文。"`)},
		{Role: "user", Content: raw(`"第一個問題"`)},
		{Role: "assistant", Content: raw(`"第一個回答"`)},
		{Role: "user", Content: raw(`"第二個問題，這是最新的一則訊息"`)},
	}
	st := BuildState(msgs, "這是最終回覆，含有足夠長的內容會被截斷", testScoring())

	// System prompt: at most marker + 20 runes, keeping the tail.
	if !strings.HasPrefix(st.SystemPromptExcerpt, truncMark) {
		t.Fatalf("system excerpt missing truncation marker: %q", st.SystemPromptExcerpt)
	}
	if n := utf8.RuneCountInString(st.SystemPromptExcerpt); n > len(truncMark)+20 {
		t.Fatalf("system excerpt too long: %d runes", n)
	}
	if !strings.HasSuffix(st.SystemPromptExcerpt, "請用繁體中文。") {
		t.Fatalf("system excerpt lost its tail: %q", st.SystemPromptExcerpt)
	}
	// Last user message: marker + last 10 runes.
	if !strings.HasPrefix(st.UserLastMessage, truncMark) ||
		!strings.HasSuffix(st.UserLastMessage, "一則訊息") {
		t.Fatalf("user last message = %q", st.UserLastMessage)
	}
	// ContextTurns=2 keeps only the last two non-system messages.
	if got := strings.Count(st.Conversation, "user:"); got != 1 {
		t.Fatalf("conversation has %d user turns, want 1", got)
	}
	if !strings.Contains(st.Conversation, "assistant: 第一個回答") ||
		strings.Contains(st.Conversation, "第一個問題") {
		t.Fatalf("conversation kept wrong turns: %q", st.Conversation)
	}
	// Reply: marker + last 5 runes.
	if !strings.HasPrefix(st.AssistantReply, truncMark) ||
		!strings.HasSuffix(st.AssistantReply, "被截斷") {
		t.Fatalf("reply = %q", st.AssistantReply)
	}
}

func TestBuildStateShortInputsUntouched(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: raw(`"短"`)},
		{Role: "user", Content: raw(`"嗨"`)},
	}
	st := BuildState(msgs, "好", testScoring())
	if st.SystemPromptExcerpt != "短" || st.UserLastMessage != "嗨" ||
		st.AssistantReply != "好" || strings.Contains(st.Conversation, truncMark) {
		t.Fatalf("short inputs were modified: %+v", st)
	}
}

func TestChatMessageText(t *testing.T) {
	if got := (ChatMessage{Content: raw(`"純字串"`)}).Text(); got != "純字串" {
		t.Fatalf("string content = %q", got)
	}
	parts := ChatMessage{Content: raw(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)}
	if got := parts.Text(); got != "ab" {
		t.Fatalf("part content = %q", got)
	}
	if got := (ChatMessage{Content: raw(`null`)}).Text(); got != "" {
		t.Fatalf("null content = %q", got)
	}
}
