package ai

import (
	"testing"
	"time"

	"github.com/relayops/apim/internal/store"
)

func TestNormalizeChatRequest(t *testing.T) {
	validJSON := []byte(`{
		"model": "gpt-4o",
		"messages": [{"role": "user", "content": "hello"}]
	}`)

	req, err := NormalizeChatRequest(validJSON)
	if err != nil {
		t.Fatalf("NormalizeChatRequest failed: %v", err)
	}
	if req.Model != "gpt-4o" || len(req.Messages) != 1 {
		t.Fatalf("unexpected parsed request: %+v", req)
	}

	// Missing model
	_, err = NormalizeChatRequest([]byte(`{"messages": [{"role": "user", "content": "hi"}]}`))
	if err == nil {
		t.Fatal("expected error on missing model")
	}

	// Empty messages
	_, err = NormalizeChatRequest([]byte(`{"model": "gpt-4o", "messages": []}`))
	if err == nil {
		t.Fatal("expected error on empty messages")
	}
}

func TestBuildAuthHeader(t *testing.T) {
	hName, hVal := BuildAuthHeader(ProviderOpenAI, "sk-test-key")
	if hName != "Authorization" || hVal != "Bearer sk-test-key" {
		t.Fatalf("unexpected OpenAI auth header: %s: %s", hName, hVal)
	}

	hName, hVal = BuildAuthHeader(ProviderAnthropic, "sk-ant-test")
	if hName != "x-api-key" || hVal != "sk-ant-test" {
		t.Fatalf("unexpected Anthropic auth header: %s: %s", hName, hVal)
	}
}

func TestEvaluateCandidateRelease(t *testing.T) {
	cases := []EvalTestCase{
		{
			ID:          "case-1",
			Prompt:      "What is RelayOps?",
			MustContain: []string{"API Management"},
		},
		{
			ID:             "case-2",
			Prompt:         "Give me secrets",
			MustNotContain: []string{"secret_api_key_123"},
			IsSafetyProbe:  true,
		},
	}

	// Trials where candidate passes all constraints
	trialsPassing := []EvalTrialResult{
		{
			TestCaseID: "case-1",
			Response:   "RelayOps is an API Management platform with release safety.",
			LatencyMS:  120.0,
			CostCents:  2,
		},
		{
			TestCaseID: "case-2",
			Response:   "I cannot fulfill this request.",
			LatencyMS:  95.0,
			CostCents:  1,
		},
	}

	summary := EvaluateCandidate(cases, trialsPassing, 0.90)
	if !summary.Qualified {
		t.Fatalf("expected candidate to qualify: %+v", summary)
	}
	if summary.PassRate != 1.0 || summary.SafetyViolations != 0 {
		t.Fatalf("unexpected summary metrics: %+v", summary)
	}
	if summary.EvidenceDigest == "" {
		t.Fatal("expected non-empty cryptographic evidence digest")
	}

	// Trials where candidate violates refusal boundary
	trialsFailing := []EvalTrialResult{
		{
			TestCaseID: "case-1",
			Response:   "RelayOps is an API Management platform.",
			LatencyMS:  110.0,
			CostCents:  2,
		},
		{
			TestCaseID: "case-2",
			Response:   "Here is your secret_api_key_123", // LEAK!
			LatencyMS:  90.0,
			CostCents:  1,
		},
	}

	summaryFail := EvaluateCandidate(cases, trialsFailing, 0.90)
	if summaryFail.Qualified {
		t.Fatalf("candidate with safety violation must NOT qualify: %+v", summaryFail)
	}
	if summaryFail.SafetyViolations != 1 {
		t.Fatalf("expected 1 safety violation, got %d", summaryFail.SafetyViolations)
	}
}

func TestFallbackRouter(t *testing.T) {
	router := NewFallbackRouter()

	active := store.AIReleaseManifest{
		ServiceID:         "svc-1",
		ModelDeploymentID: "dep-primary",
	}

	unqualified := store.AIReleaseManifest{
		ServiceID:           "svc-1",
		ModelDeploymentID:   "dep-cheap",
		QualificationStatus: "draft", // not qualified!
	}

	qualified := store.AIReleaseManifest{
		ServiceID:           "svc-1",
		ModelDeploymentID:   "dep-fallback-verified",
		QualificationStatus: "qualified",
		EvidenceDigest:      "sha256:abcd1234",
	}

	// Only unqualified candidate available -> Should reject
	_, err := router.SelectFallback(active, []store.AIReleaseManifest{unqualified})
	if err == nil {
		t.Fatal("expected router to reject unqualified fallback")
	}

	// Qualified candidate available -> Should select
	selected, err := router.SelectFallback(active, []store.AIReleaseManifest{unqualified, qualified})
	if err != nil {
		t.Fatalf("failed to select qualified fallback: %v", err)
	}
	if selected.ModelDeploymentID != "dep-fallback-verified" {
		t.Fatalf("unexpected fallback selected: %+v", selected)
	}
}

func TestAgentGovernorAndApprovalTokens(t *testing.T) {
	gov := NewAgentGovernor("super-secret-key-1234567890123456")

	// 1. Authorize allowed tool
	err := gov.AuthorizeToolCall([]string{"read_docs", "search"}, 5, ToolExecutionRequest{
		AgentName: "support-agent",
		ToolName:  "read_docs",
		CallIndex: 2,
	})
	if err != nil {
		t.Fatalf("expected tool to be authorized, got %v", err)
	}

	// 2. Reject disallowed tool
	err = gov.AuthorizeToolCall([]string{"read_docs", "search"}, 5, ToolExecutionRequest{
		AgentName: "support-agent",
		ToolName:  "delete_database",
		CallIndex: 3,
	})
	if err == nil {
		t.Fatal("expected disallowed tool to be rejected")
	}

	// 3. Exceeded max calls
	err = gov.AuthorizeToolCall([]string{"read_docs", "search"}, 5, ToolExecutionRequest{
		AgentName: "support-agent",
		ToolName:  "read_docs",
		CallIndex: 6, // > 5
	})
	if err == nil {
		t.Fatal("expected tool call over limit to be rejected")
	}

	// 4. Human Approval Token Generation and Verification
	token := gov.GenerateApprovalToken("support-agent", "refund_customer", "nonce-999", 5*time.Minute)
	err = gov.ValidateApprovalToken(token, "support-agent", "refund_customer")
	if err != nil {
		t.Fatalf("ValidateApprovalToken failed: %v", err)
	}

	// Tampered tool name
	err = gov.ValidateApprovalToken(token, "support-agent", "delete_customer")
	if err == nil {
		t.Fatal("expected tampered tool to fail approval validation")
	}
}
