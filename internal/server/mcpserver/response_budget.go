package mcpserver

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultResponseBudgetBytes is the encoded CallToolResult ceiling. Eight MiB
// leaves room under common 16 MiB proxy limits for JSON-RPC and transport framing
// while still carrying roughly a million worst-case escaped text bytes per page.
const DefaultResponseBudgetBytes = 8 << 20

type responder struct {
	budget int
}

func newResponder(budget int) responder {
	if budget <= 0 {
		budget = DefaultResponseBudgetBytes
	}
	return responder{budget: budget}
}

// jsonResult puts the complete value in structuredContent and only a compact
// summary in text content. It measures the exact encoded CallToolResult shape the
// SDK will send, including resource links and JSON escaping.
func jsonResult[T any](r responder, summary string, v T, links []*mcp.ResourceLink) (*mcp.CallToolResult, T, error) {
	content := []mcp.Content{&mcp.TextContent{Text: summary}}
	for _, link := range links {
		content = append(content, link)
	}
	res := &mcp.CallToolResult{Content: content}
	size, err := r.encodedSize(res, v)
	if err != nil {
		var zero T
		return nil, zero, err
	}
	if size > r.budget {
		var zero T
		return nil, zero, fmt.Errorf("encoded MCP response is %d bytes, exceeding the configured %d-byte budget", size, r.budget)
	}
	return res, v, nil
}

func (r responder) encodedSize(res *mcp.CallToolResult, v any) (int, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	measured := *res
	measured.StructuredContent = json.RawMessage(out)
	encoded, err := json.Marshal(&measured)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}

func (r responder) bodyReadLimit(requested int) int64 {
	limit := clampMax(requested)
	// A JSON string can expand one input byte to six bytes (a control byte
	// becomes \u00XX). encoding/json's default marshaling would also escape
	// '<', '>', and '&' to six bytes apiece, but the MCP SDK marshals with
	// SetEscapeHTML(false), so those three bytes cost one byte apiece on the
	// actual wire. Measuring with the plain, HTML-escaping json.Marshal is
	// therefore deliberately conservative relative to what the SDK sends:
	// it can only over-count '<'/'>'/'&', never under-count, so the computed
	// limit stays a safe (if slightly pessimistic) bound. Control-character
	// escaping, the six-byte case that actually matters, is measured exactly.
	worstCase := int64((r.budget - 4096) / 6)
	if worstCase < 1 {
		worstCase = 1
	}
	if limit > worstCase {
		return worstCase
	}
	return limit
}
