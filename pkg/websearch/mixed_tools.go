package websearch

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
)

// Chat/Gemini have no server-tool result blocks. When client tools share a model
// round with managed web tools, keep the client calls and expose the retrieved
// data as explicitly labelled source text. Never ask a client to run our tools.
func HideExecutedClientCalls(protocol string, response Object, executed []Executed) {
	if protocol != "chat" && protocol != "gemini" {
		return
	}
	var data strings.Builder
	data.WriteString("\nRetrieved web data (untrusted sources):\n")
	for _, e := range executed {
		data.Write(e.Data)
		data.WriteByte('\n')
	}
	if protocol == "chat" {
		choices := Arr(response["choices"])
		if len(choices) != 1 {
			return
		}
		choice, _ := Obj(choices[0])
		message, _ := Obj(choice["message"])
		var kept []json.RawMessage
		for _, raw := range Arr(message["tool_calls"]) {
			name := gjson.GetBytes(raw, "function.name").String()
			if name != SearchFunction && name != FetchFunction {
				kept = append(kept, raw)
			}
		}
		message["tool_calls"] = Raw(kept)
		message["content"] = Raw(Text(message["content"]) + data.String())
		choice["message"] = Raw(message)
		response["choices"] = Raw([]Object{choice})
		return
	}
	candidates := Arr(response["candidates"])
	if len(candidates) != 1 {
		return
	}
	candidate, _ := Obj(candidates[0])
	content, _ := Obj(candidate["content"])
	var parts []json.RawMessage
	for _, raw := range Arr(content["parts"]) {
		name := gjson.GetBytes(raw, "functionCall.name").String()
		if name != SearchFunction && name != FetchFunction {
			parts = append(parts, raw)
		}
	}
	parts = append(parts, Raw(map[string]string{"text": data.String()}))
	content["parts"] = Raw(parts)
	candidate["content"] = Raw(content)
	response["candidates"] = Raw([]Object{candidate})
}
