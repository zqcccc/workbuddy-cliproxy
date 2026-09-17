package main

import (
	"strings"
	"testing"
)

func TestTrailingChunkOnlyForReasoningOnlyStreams(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"reasoning only", `{"choices":[{"delta":{"reasoning_content":"hmm"}}]}`, true},
		{"reasoning then text", `{"choices":[{"delta":{"reasoning_content":"hmm"}}]}
{"choices":[{"delta":{"content":"hi"}}]}`, false},
		{"text only", `{"choices":[{"delta":{"content":"hi"}}]}`, false},
		{"tool call only", `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"bash"}}]}}]}`, false},
		{"empty stream", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var st upstreamStreamState
			for _, line := range strings.Split(tc.payload, "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				st.observe(line)
			}
			got := st.trailingChunk()
			if tc.want && got == "" {
				t.Fatalf("expected a trailing chunk, got none")
			}
			if !tc.want && got != "" {
				t.Fatalf("expected no trailing chunk, got %s", got)
			}
		})
	}
}

func TestTrailingChunkShape(t *testing.T) {
	var st upstreamStreamState
	st.observe(`{"id":"resp_1","model":"deepseek-v4.1-flash","created":1700000000,"choices":[{"delta":{"reasoning_content":"hmm"}}]}`)
	st.observe(`{"id":"resp_1","model":"deepseek-v4.1-flash","created":1700000000,"choices":[{"finish_reason":"length","delta":{}}]}`)

	got := st.trailingChunk()
	for _, want := range []string{`"content":" "`, `"finish_reason":"length"`, `"id":"resp_1"`, `"deepseek-v4.1-flash"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("trailing chunk %s missing %s", got, want)
		}
	}
}

func TestAggregateSSEAppendsTrailingChunkOnce(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"r","choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	chunks := aggregateSSE(strings.NewReader(stream), true)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks (reasoning + synthetic), got %d: %v", len(chunks), chunks)
	}
	last := string(chunks[len(chunks)-1].Payload)
	if !strings.HasPrefix(last, "data: ") {
		t.Fatalf("synthetic chunk must be SSE framed, got %q", last)
	}
	if !strings.Contains(last, `"finish_reason":"length"`) {
		t.Fatalf("synthetic chunk should report the truncated turn as length: %s", last)
	}
}

func TestAggregateSSELeavesNativeChatPathAlone(t *testing.T) {
	stream := `data: {"id":"r","choices":[{"delta":{"reasoning_content":"thinking"}}]}` + "\n\n"
	chunks := aggregateSSE(strings.NewReader(stream), false)
	if len(chunks) != 1 {
		t.Fatalf("native chat-completions path must not gain a synthetic chunk, got %d", len(chunks))
	}
}
