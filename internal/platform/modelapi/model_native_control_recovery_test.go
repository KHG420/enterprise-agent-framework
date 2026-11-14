package modelapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentcontract "github.com/KHG420/agenstra/internal/contract/agent"
	reactcore "github.com/KHG420/agenstra/internal/runtime/react"
)

func TestNativeControlSchemaRecoveryUsesMeasuredSingleDecision(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, corrected := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/corrected=%t", stream, corrected), func(t *testing.T) {
				inputLengths := []int64{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					inputLengths = append(inputLengths, int64(len(body)+128))
					var payload agentcontract.JSON
					if err := json.Unmarshal(body, &payload); err != nil {
						t.Error(err)
						return
					}
					name, arguments := "inspect_capability", `{"capability":"records.read"}`
					if len(inputLengths) == 2 {
						assertSingleDecisionOutputRequest(t, payload)
						prompt := payload["messages"].([]any)[0].(agentcontract.JSON)["content"].(string)
						if !strings.Contains(prompt, "required") || !strings.Contains(prompt, "name") {
							t.Error("recovery lost the missing control parameter")
						}
						name = "submit_decision"
						arguments = `{"kind":"inspect_capability","capability":"records.read"}`
						if corrected {
							arguments = `{"kind":"final","answer_markdown":"The requested inputs are incomplete.","fact_ids":[]}`
						}
					}
					call := agentcontract.JSON{"index": 0, "type": "function", "function": agentcontract.JSON{"name": name, "arguments": arguments}}
					if stream {
						streamFrame(t, w, agentcontract.JSON{"choices": []any{agentcontract.JSON{"index": 0, "delta": agentcontract.JSON{"tool_calls": []any{call}}, "finish_reason": "tool_calls"}}})
						if _, err := fmt.Fprint(w, "data: [DONE]\n\n"); err != nil {
							t.Error(err)
						}
						return
					}
					if err := json.NewEncoder(w).Encode(agentcontract.JSON{"choices": []any{agentcontract.JSON{
						"message": agentcontract.JSON{"tool_calls": []any{call}}, "finish_reason": "tool_calls",
					}}}); err != nil {
						t.Error(err)
					}
				}))
				defer server.Close()
				model, err := NewHTTPJSONDecisionModel("test", server.URL, "fixture-key", time.Second, server.Client())
				if err != nil {
					t.Fatal(err)
				}
				model.DecisionOutputMode, model.StreamAnswers = "native_tools", stream
				provider := &coreTestProvider{caps: map[string]agentcontract.CapabilityDescription{}}
				runtime := &reactcore.AgentRuntime{Provider: provider, Model: model, MaxModelRounds: 3}
				result, err := runtime.Run(t.Context(), "Explain missing inputs without running business operations")
				if err != nil || len(inputLengths) != 2 || len(result.ModelCalls) != 2 || provider.called != 0 {
					t.Fatal("control recovery changed the bounded attempts or invoked business work", result, err)
				}
				status := "failed"
				if corrected {
					status = "completed"
				}
				if result.Status != status || result.ModelCalls[0].FormatError != "model_decision_schema_invalid" || !result.ModelCalls[1].FormatRecovery || result.ModelUsage.FormatRecoveryRequests != 1 {
					t.Fatal("control recovery changed strict rejection or format accounting", result)
				}
				for i, call := range result.ModelCalls {
					if call.EstimatedInputTokens != inputLengths[i] {
						t.Fatal("recovery measurement differs from the actual channel payload")
					}
				}
				if model.DecisionOutputMode != "native_tools" {
					t.Fatal("recovery mutated the shared adapter")
				}
			})
		}
	}
}
