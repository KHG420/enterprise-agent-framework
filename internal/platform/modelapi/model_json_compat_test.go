package modelapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KHG420/agenstra/internal/base/jsonvalue"
	agentcontract "github.com/KHG420/agenstra/internal/contract/agent"
	reactcore "github.com/KHG420/agenstra/internal/runtime/react"
)

func TestNormalizeModelJSONPreservesCompleteObjectBytes(t *testing.T) {
	object := `{"body":{"number":9007199254740993,"decimal":0.000000000000000001,"zero":0,"flag":false,"missing":null,"text":"]}","items":[1,2]}}`
	for _, suffix := range []string{"]}", " } ] } \n"} {
		got, changed := normalizeModelJSON([]byte(object + suffix))
		if !changed || string(got) != object {
			t.Fatal("redundant suffix was not removed with object bytes intact", string(got), changed)
		}
	}
	for _, body := range []string{
		object, object + "\n", object + `{}`, object + `] {"other":1}`,
		object + `]} prose`, object + strings.Repeat("}", 33),
		`{"body":`, `{"body":{"id":}}]}`, `{"body":,}]}`,
		`[{"body":1}]]`, `null]}`, "```json\n" + object + "\n```]}",
	} {
		got, changed := normalizeModelJSON([]byte(body))
		if changed || string(got) != body {
			t.Fatal("normalization accepted an unsupported suffix or changed an incomplete value", body)
		}
	}
}

func TestNormalizeModelJSONFormattingAndStringContents(t *testing.T) {
	for _, test := range []struct{ input, expected string }{
		{"\xef\xbb\xbf{\"a\":1}", `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{`{"a":[1,2,],"b":false,"c":null,}`, `{"a":[1,2],"b":false,"c":null}`},
		{`{"calls":[{"arguments":{"id":1}},{"arguments":{"id":2}}}]}`, `{"calls":[{"arguments":{"id":1}},{"arguments":{"id":2}}]}`},
		{"{\"a\":\"line\none\t中文\r\"}", `{"a":"line\u000aone\u0009中文\u000d"}`},
		{`{"a":"escaped \\\" quotes,}]", "b":[0,],}]} `, `{"a":"escaped \\\" quotes,}]", "b":[0]}`},
		{"\xef\xbb\xbf```json\n{\"a\":[1,],}\n```", `{"a":[1]}`},
	} {
		got, changed := normalizeModelJSON([]byte(test.input))
		if !changed || string(got) != test.expected || modelJSONFormatError(got) != "" {
			t.Fatal("format normalization changed the intended complete value", test.input, string(got))
		}
	}
	for _, input := range []string{
		`{"a":[,]}`, `{"a":[1,,]}`, `{"a":,}`, `{"a":1,,}`,
		`{"a":'text'}`, `{"a":"bad\escape"}`, `{"a":NaN}`,
		`{"a":1, "b":`, `{"a":"unterminated`, "{\"a\":\"backslash\\\n\"}",
		"Before\n```json\n{\"a\":1}\n```", "```json\n{\"a\":1}\n```\nAfter",
		`{"a":1} {"b":2}`, `{"a":1 /* comment */}`,
		`{"a":[1}}`, `{"a":{"id":1]]}`, `{"a":[1` + strings.Repeat("}", 33) + `]}`,
	} {
		got, changed := normalizeModelJSON([]byte(input))
		if changed || string(got) != input {
			t.Fatal("unsupported or incomplete data was transformed", input, string(got))
		}
	}
}

func TestNormalizeModelJSONLogsOnlyFormatMetadata(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	_, changed := normalizeModelJSON([]byte(`{"private-key":"private-value",}]} `))
	if !changed || !strings.Contains(output.String(), "normalized_bytes=") || !strings.Contains(output.String(), "extra_closing_delimiters") || strings.Contains(output.String(), "private-key") || strings.Contains(output.String(), "private-value") {
		t.Fatal("normalization logs exposed values or omitted format metadata", output.String())
	}
}

func TestNormalizeModelJSONBoundsControlCharacterExpansion(t *testing.T) {
	body := "{\"text\":\"" + strings.Repeat("\n", (8<<20)/6+1) + "\"}"
	got, changed := normalizeModelJSON([]byte(body))
	if changed || string(got) != body {
		t.Fatal("control-character normalization exceeded the existing response bound")
	}
}

func TestModelJSONSuffixCompatibilityUsesStrictDecisionAndApproval(t *testing.T) {
	arguments := `{"body":{"number":9007199254740993,"zero":0,"flag":false,"missing":null,"text":"]}","items":[1,2]}}`
	call := `{"capability":"notice.create","arguments":` + arguments + `,"reason":"Prepare the requested notice"}`
	for _, channel := range []string{"json_object", "json_schema", "output_tools", "native_tools", "native_tools_stream"} {
		mode := strings.TrimSuffix(channel, "_stream")
		stream := strings.HasSuffix(channel, "_stream")
		for _, invalid := range []string{"", "mismatched close", "duplicate key", "schema"} {
			t.Run(channel+"/"+invalid, func(t *testing.T) {
				requests := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					body := `{"schema":"agenstra.decision.v1","kind":"tool_batch","calls":[` + call + `]}`
					name := "submit_decision"
					if mode == "output_tools" {
						body = `{"kind":"tool_batch","calls":[` + call + `]}`
					} else if mode == "native_tools" {
						name, body = "tool_batch", `{"calls":[`+call+`]}`
					} else {
						body = strings.ReplaceAll(body, `"capability":`, `"call_ref":"write-1","capability":`)
					}
					if invalid == "mismatched close" {
						body = strings.TrimSuffix(body, "]}") + "}]}"
					} else if invalid == "duplicate key" {
						body = strings.TrimSuffix(body, "}") + `,"calls":[]}`
					} else if invalid == "schema" {
						body = strings.Replace(body, `"reason":"Prepare the requested notice"`, `"reason":3`, 1)
					}
					body += "]}"
					message := agentcontract.JSON{"content": body}
					if mode == "output_tools" || mode == "native_tools" {
						message = agentcontract.JSON{"tool_calls": []any{agentcontract.JSON{"type": "function", "function": agentcontract.JSON{"name": name, "arguments": body}}}}
					}
					if stream {
						message["tool_calls"].([]any)[0].(agentcontract.JSON)["index"] = 0
						w.Header().Set("Content-Type", "text/event-stream")
						streamFrame(t, w, agentcontract.JSON{"choices": []any{agentcontract.JSON{"index": 0, "delta": message, "finish_reason": "tool_calls"}}})
						streamFrame(t, w, agentcontract.JSON{"choices": []any{}, "usage": agentcontract.JSON{"prompt_tokens": 40, "completion_tokens": 80}})
						if _, err := w.Write([]byte("data: [DONE]\n\n")); err != nil {
							t.Error(err)
						}
						return
					}
					if err := json.NewEncoder(w).Encode(agentcontract.JSON{"choices": []any{agentcontract.JSON{"message": message, "finish_reason": "tool_calls"}}, "usage": agentcontract.JSON{"prompt_tokens": 40, "completion_tokens": 80}}); err != nil {
						t.Error(err)
					}
				}))
				defer upstream.Close()
				model, err := NewHTTPJSONDecisionModel("compatible", upstream.URL, "test", time.Second, upstream.Client())
				if err != nil {
					t.Fatal(err)
				}
				model.DecisionOutputMode, model.MaxAttempts = mode, 1
				model.StreamAnswers = stream
				provider := &coreTestProvider{caps: map[string]agentcontract.CapabilityDescription{
					"notice.create": {Name: "notice.create", InputSchema: agentcontract.JSON{"type": "object", "properties": agentcontract.JSON{"body": agentcontract.JSON{"type": "object"}}, "required": []string{"body"}, "additionalProperties": false}, Effect: "write", ApprovalRequired: true},
				}}
				runtime := &reactcore.AgentRuntime{Provider: provider, Model: model, Grants: map[string]bool{"notice.create": true}, MaxModelRounds: 1}
				state, err := runtime.NewState("Prepare a notice", "")
				if err != nil {
					t.Fatal(err)
				}
				if err := runtime.Step(t.Context(), state, nil); err != nil {
					t.Fatal(err)
				}
				if requests != 1 || state.RoundsUsed != 1 || state.ModelUsage.FormatRecoveryRequests != 0 || provider.called != 0 || len(state.Facts) != 0 || state.ModelUsage.OutputTokens != 80 {
					t.Fatal("compatibility replayed a model call, lost usage or executed before approval", state)
				}
				if invalid == "duplicate key" || invalid == "schema" {
					if state.Status != "failed" || len(state.Pending) != 0 || len(state.Decisions) != 0 || state.ModelUsage.InvalidResponses != 1 {
						t.Fatal("normalization bypassed duplicate-key or schema rejection", state)
					}
					return
				}
				if len(state.Pending) != 1 || state.ModelUsage.InvalidResponses != 0 {
					t.Fatal("complete normalized call did not reach normal approval", state)
				}
				var expected agentcontract.JSON
				if err := jsonvalue.DecodeStrict([]byte(arguments), &expected); err != nil {
					t.Fatal(err)
				}
				want, err := agentcontract.CanonicalJSON(expected)
				if err != nil {
					t.Fatal(err)
				}
				got, err := agentcontract.CanonicalJSON(state.Pending[0].Call.Arguments)
				if err != nil {
					t.Fatal(err)
				}
				if string(want) != string(got) {
					t.Fatal("normalized call changed values or number precision", string(got))
				}
			})
		}
	}
}

func TestEmptyModelToolArgumentsFollowDeclaredSchema(t *testing.T) {
	for _, body := range []string{"", " \n", "null"} {
		for _, required := range []bool{false, true} {
			schema := agentcontract.JSON{"type": "object", "properties": agentcontract.JSON{"id": agentcontract.JSON{"type": "integer"}}, "additionalProperties": false}
			if required {
				schema["required"] = []string{"id"}
			}
			got, changed := normalizeModelToolJSON([]byte(body), schema)
			if required {
				if changed || string(got) != body {
					t.Fatal("missing required arguments became an empty call")
				}
			} else if !changed || string(got) != "{}" {
				t.Fatal("declared optional parameters lost empty-object compatibility")
			}
		}
	}
}

func TestEmptyNativeReviewArgumentsUseOneValidatedRequest(t *testing.T) {
	for _, body := range []string{"", "null"} {
		requests := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			call := agentcontract.JSON{"type": "function", "function": agentcontract.JSON{"name": "approve_completion", "arguments": body}}
			if err := json.NewEncoder(w).Encode(agentcontract.JSON{"choices": []any{agentcontract.JSON{"message": agentcontract.JSON{"tool_calls": []any{call}}, "finish_reason": "tool_calls"}}, "usage": agentcontract.JSON{"prompt_tokens": 40, "completion_tokens": 2}}); err != nil {
				t.Error(err)
			}
		}))
		model, err := NewHTTPJSONDecisionModel("compatible", upstream.URL, "test", time.Second, upstream.Client())
		if err != nil {
			t.Fatal(err)
		}
		model.DecisionOutputMode = "native_tools"
		packet := agentcontract.ContextPacket{Schema: "agenstra.context.v1", CompletionReview: &agentcontract.Decision{Kind: "final", AnswerMarkdown: "verified", FactIDs: []string{}}}
		decision, err := model.Decide(t.Context(), packet, "Review the saved proposal")
		upstream.Close()
		if err != nil || decision.Kind != "approve_completion" || requests != 1 || decision.ModelCall.FormatError != "" || !decision.ModelCall.UsageAvailable || decision.ModelCall.OutputTokens != 2 {
			t.Fatal("empty-argument compatibility lost the review contract or repeated a request", decision, err, requests)
		}
	}
}
