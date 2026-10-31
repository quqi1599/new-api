package websearch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// Snapshot retains a whole same-protocol response, including opaque reasoning
// and unrelated function calls. It never turns a failed stream into a success.
func Snapshot(protocol string, body []byte) (Object, error) {
	if len(body) > 32<<20 {
		return nil, errors.New("managed web model response exceeds 32 MiB")
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		if e := gjson.GetBytes(body, "error"); e.Exists() && e.Type != gjson.Null {
			return nil, errors.New("model returned an error response")
		}
		return Obj(body)
	}
	frames, err := sseFrames(body)
	if err != nil {
		return nil, err
	}
	var response Object
	var blocks []Object
	var arguments = map[int]string{}
	var chatTools = map[int]Object{}
	message := Object{"role": Raw("assistant")}
	text, reasoning := "", ""
	for _, data := range frames {
		if string(data) == "[DONE]" {
			continue
		}
		o, err := Obj(data)
		if err != nil {
			return nil, errors.New("invalid model SSE JSON")
		}
		if v := o["error"]; len(v) > 0 && string(v) != "null" {
			return nil, errors.New("model stream reported an error")
		}
		switch protocol {
		case "responses":
			typ := Text(o["type"])
			if typ == "response.failed" || typ == "error" {
				return nil, errors.New("model Responses stream failed")
			}
			if typ == "response.completed" || typ == "response.incomplete" {
				response, err = Obj(o["response"])
				if err != nil {
					return nil, err
				}
			}
		case "claude":
			switch Text(o["type"]) {
			case "error":
				return nil, errors.New("model Messages stream failed")
			case "message_start":
				response, err = Obj(o["message"])
				if err != nil {
					return nil, err
				}
			case "content_block_start":
				idx := int(gjson.GetBytes(data, "index").Int())
				if idx < 0 || idx > 1024 {
					return nil, errors.New("invalid content index")
				}
				for len(blocks) <= idx {
					blocks = append(blocks, Object{})
				}
				blocks[idx], err = Obj(o["content_block"])
				if err != nil {
					return nil, err
				}
			case "content_block_delta":
				idx := int(gjson.GetBytes(data, "index").Int())
				if idx < 0 || idx >= len(blocks) {
					return nil, errors.New("delta without content block")
				}
				d, err := Obj(o["delta"])
				if err != nil {
					return nil, err
				}
				switch Text(d["type"]) {
				case "text_delta":
					blocks[idx]["text"] = Raw(Text(blocks[idx]["text"]) + Text(d["text"]))
				case "thinking_delta":
					blocks[idx]["thinking"] = Raw(Text(blocks[idx]["thinking"]) + Text(d["thinking"]))
				case "signature_delta":
					blocks[idx]["signature"] = Raw(Text(blocks[idx]["signature"]) + Text(d["signature"]))
				case "input_json_delta":
					arguments[idx] += Text(d["partial_json"])
				case "citations_delta":
					v := Arr(blocks[idx]["citations"])
					v = append(v, d["citation"])
					blocks[idx]["citations"] = Raw(v)
				default:
					return nil, errors.New("unsupported Messages content delta during managed web search")
				}
			case "message_delta":
				if response == nil {
					return nil, errors.New("message delta without start")
				}
				d, _ := Obj(o["delta"])
				for k, v := range d {
					response[k] = v
				}
				u, _ := Obj(response["usage"])
				if u == nil {
					u = Object{}
				}
				inc, _ := Obj(o["usage"])
				for k, v := range inc {
					u[k] = v
				}
				response["usage"] = Raw(u)
			}
		case "gemini":
			if response == nil {
				response = o
			} else {
				for k, v := range o {
					if k != "candidates" {
						response[k] = v
					}
				}
			}
			candidate := gjson.GetBytes(data, "candidates.0")
			for _, part := range candidate.Get("content.parts").Array() {
				b, err := Obj([]byte(part.Raw))
				if err != nil {
					return nil, err
				}
				blocks = append(blocks, b)
			}
			if candidate.Get("finishReason").Exists() {
				response["candidates"] = o["candidates"]
			}
		default:
			if response == nil {
				response = Object{"id": o["id"], "object": Raw("chat.completion"), "model": o["model"], "created": o["created"]}
			}
			if v := o["usage"]; len(v) > 0 && string(v) != "null" {
				response["usage"] = v
			}
			choices := gjson.GetBytes(data, "choices").Array()
			if len(choices) > 1 {
				return nil, errors.New("managed web search requires one completion")
			}
			if len(choices) == 0 {
				continue
			}
			choice := choices[0]
			if f := choice.Get("finish_reason"); f.Exists() && f.Type != gjson.Null {
				response["_finish"] = Raw(f.String())
			}
			d := choice.Get("delta")
			text += d.Get("content").String()
			reasoning += d.Get("reasoning_content").String()
			for _, tc := range d.Get("tool_calls").Array() {
				idx := int(tc.Get("index").Int())
				if idx < 0 || idx > 128 {
					return nil, errors.New("invalid tool call index")
				}
				t := chatTools[idx]
				if t == nil {
					t = Object{"type": Raw("function")}
					chatTools[idx] = t
				}
				if id := tc.Get("id"); id.Exists() {
					t["id"] = Raw(id.String())
				}
				f, _ := Obj(t["function"])
				if f == nil {
					f = Object{}
				}
				if n := tc.Get("function.name"); n.Exists() {
					f["name"] = Raw(n.String())
				}
				f["arguments"] = Raw(Text(f["arguments"]) + tc.Get("function.arguments").String())
				t["function"] = Raw(f)
			}
			for k, v := range d.Map() {
				switch k {
				case "role", "content", "reasoning_content", "tool_calls":
				default:
					if v.Exists() {
						message[k] = []byte(v.Raw)
					}
				}
			}
		}
	}
	if response == nil {
		return nil, errors.New("model stream has no terminal response")
	}
	switch protocol {
	case "claude":
		for i, s := range arguments {
			if !gjson.Valid(s) {
				return nil, errors.New("invalid model tool JSON")
			}
			blocks[i]["input"] = []byte(s)
		}
		response["content"] = Raw(blocks)
		if Text(response["stop_reason"]) == "" {
			return nil, errors.New("Messages stream ended without stop reason")
		}
	case "chat":
		message["content"] = Raw(text)
		if reasoning != "" {
			message["reasoning_content"] = Raw(reasoning)
		}
		var calls []Object
		for i := 0; i < len(chatTools); i++ {
			if chatTools[i] == nil {
				return nil, errors.New("noncontiguous tool calls")
			}
			calls = append(calls, chatTools[i])
		}
		if len(calls) > 0 {
			message["tool_calls"] = Raw(calls)
		}
		if Text(response["_finish"]) == "" {
			return nil, errors.New("Chat stream ended without finish reason")
		}
		response["choices"] = Raw([]any{map[string]any{"index": 0, "message": message, "finish_reason": response["_finish"]}})
		delete(response, "_finish")
	case "gemini":
		candidates := Arr(response["candidates"])
		if len(candidates) != 1 {
			return nil, errors.New("managed web search requires one Gemini candidate")
		}
		c, _ := Obj(candidates[0])
		c["content"] = Raw(map[string]any{"role": "model", "parts": blocks})
		response["candidates"] = Raw([]Object{c})
	}
	return response, nil
}

func sseFrames(body []byte) ([][]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 4096), 32<<20)
	var frames [][]byte
	var data []string
	flush := func() {
		if len(data) > 0 {
			frames = append(frames, []byte(strings.Join(data, "\n")))
			data = nil
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return frames, scanner.Err()
}

// Emit writes a valid stream from a verified complete response. Intermediate
// generations stay private while their server-side tools are being executed.
func Emit(w io.Writer, protocol string, response Object) error {
	write := func(typ string, v any) error {
		b, err := common.Marshal(v)
		if err != nil {
			return err
		}
		if typ != "" {
			if _, err = fmt.Fprintf(w, "event: %s\n", typ); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	switch protocol {
	case "responses":
		start := Object{}
		for k, v := range response {
			start[k] = v
		}
		start["status"] = Raw("in_progress")
		start["output"] = Raw([]any{})
		delete(start, "usage")
		seq := 0
		emit := func(typ string, fields map[string]any) error {
			fields["type"] = typ
			fields["sequence_number"] = seq
			seq++
			return write(typ, fields)
		}
		if err := emit("response.created", map[string]any{"response": start}); err != nil {
			return err
		}
		for i, item := range Arr(response["output"]) {
			o, err := Obj(item)
			if err != nil {
				return err
			}
			id := Text(o["id"])
			typ := Text(o["type"])
			initial := Object{}
			for k, v := range o {
				initial[k] = v
			}
			if typ == "message" {
				initial["content"] = Raw([]any{})
				initial["status"] = Raw("in_progress")
			}
			if typ == "function_call" {
				initial["arguments"] = Raw("")
			}
			if err := emit("response.output_item.added", map[string]any{"output_index": i, "item": initial}); err != nil {
				return err
			}
			if typ == "message" {
				for j, c := range Arr(o["content"]) {
					part, _ := Obj(c)
					empty := Object{}
					for k, v := range part {
						empty[k] = v
					}
					if Text(part["type"]) == "output_text" {
						empty["text"] = Raw("")
					}
					fields := map[string]any{"item_id": id, "output_index": i, "content_index": j, "part": empty}
					if err := emit("response.content_part.added", fields); err != nil {
						return err
					}
					if Text(part["type"]) == "output_text" {
						if err := emit("response.output_text.delta", map[string]any{"item_id": id, "output_index": i, "content_index": j, "delta": Text(part["text"])}); err != nil {
							return err
						}
						if err := emit("response.output_text.done", map[string]any{"item_id": id, "output_index": i, "content_index": j, "text": Text(part["text"])}); err != nil {
							return err
						}
					}
					if err := emit("response.content_part.done", map[string]any{"item_id": id, "output_index": i, "content_index": j, "part": part}); err != nil {
						return err
					}
				}
			}
			if typ == "function_call" {
				if err := emit("response.function_call_arguments.delta", map[string]any{"item_id": id, "output_index": i, "delta": Text(o["arguments"])}); err != nil {
					return err
				}
				if err := emit("response.function_call_arguments.done", map[string]any{"item_id": id, "output_index": i, "arguments": Text(o["arguments"])}); err != nil {
					return err
				}
			}
			if err := emit("response.output_item.done", map[string]any{"output_index": i, "item": o}); err != nil {
				return err
			}
		}
		typ := "response.completed"
		if Text(response["status"]) == "incomplete" {
			typ = "response.incomplete"
		}
		return emit(typ, map[string]any{"response": response})
	case "claude":
		start := Object{}
		for k, v := range response {
			start[k] = v
		}
		start["content"] = Raw([]any{})
		start["stop_reason"] = Raw(nil)
		start["stop_sequence"] = Raw(nil)
		if err := write("message_start", map[string]any{"type": "message_start", "message": start}); err != nil {
			return err
		}
		for i, part := range Arr(response["content"]) {
			block, err := Obj(part)
			if err != nil {
				return err
			}
			initial := Object{}
			for k, v := range block {
				initial[k] = v
			}
			var deltas []any
			switch Text(block["type"]) {
			case "text":
				initial["text"] = Raw("")
				deltas = append(deltas, map[string]any{"type": "text_delta", "text": Text(block["text"])})
			case "thinking":
				initial["thinking"] = Raw("")
				initial["signature"] = Raw("")
				deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": Text(block["thinking"])}, map[string]any{"type": "signature_delta", "signature": Text(block["signature"])})
			case "tool_use", "server_tool_use":
				initial["input"] = Raw(Object{})
				deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": string(block["input"])})
			}
			if err := write("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": initial}); err != nil {
				return err
			}
			for _, delta := range deltas {
				if err := write("content_block_delta", map[string]any{"type": "content_block_delta", "index": i, "delta": delta}); err != nil {
					return err
				}
			}
			if err := write("content_block_stop", map[string]any{"type": "content_block_stop", "index": i}); err != nil {
				return err
			}
		}
		if err := write("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": response["stop_reason"], "stop_sequence": response["stop_sequence"]}, "usage": response["usage"]}); err != nil {
			return err
		}
		return write("message_stop", map[string]string{"type": "message_stop"})
	case "gemini":
		return write("", response)
	default:
		choices := gjson.GetBytes(Raw(response), "choices").Array()
		if len(choices) != 1 {
			return errors.New("invalid final Chat choices")
		}
		chunk := Object{}
		for k, v := range response {
			if k != "choices" {
				chunk[k] = v
			}
		}
		chunk["object"] = Raw("chat.completion.chunk")
		message := json.RawMessage(choices[0].Get("message").Raw)
		m, _ := Obj(message)
		calls := Arr(m["tool_calls"])
		for i, c := range calls {
			o, _ := Obj(c)
			o["index"] = Raw(i)
			calls[i] = Raw(o)
		}
		if len(calls) > 0 {
			m["tool_calls"] = Raw(calls)
		}
		chunk["choices"] = Raw([]any{map[string]any{"index": 0, "delta": m, "finish_reason": nil}})
		delete(chunk, "usage")
		if err := write("", chunk); err != nil {
			return err
		}
		chunk["choices"] = Raw([]any{map[string]any{"index": 0, "delta": Object{}, "finish_reason": json.RawMessage(choices[0].Get("finish_reason").Raw)}})
		chunk["usage"] = response["usage"]
		if err := write("", chunk); err != nil {
			return err
		}
		_, err := io.WriteString(w, "data: [DONE]\n\n")
		return err
	}
}
