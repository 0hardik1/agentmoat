// Prompt handler for `audit-cluster-for-agentic-workloads`.
//
// The prompt instructs the MCP client (and the LLM driving it) to:
//
//  1. Call scan_cluster.
//  2. Filter the resulting WorkloadResults for likely-agentic candidates
//     by matching image substrings (claude / openai / anthropic / etc.)
//     and well-known API-key env vars (ANTHROPIC_API_KEY / OPENAI_API_KEY
//     / etc.).
//  3. For each candidate, call assess_workload to get a fresh verdict.
//  4. Call propose_plan to produce a MigrationPlan (optionally with
//     include_review=true).
//  5. Summarize and surface the next concrete operator step.
//
// The heuristic lives in the prompt text rather than in Go code (a) so
// operators can edit it without recompiling and (b) because the handoff
// explicitly calls for "a sensible heuristic, not a full classifier".
// Image substrings and env-var names are deliberately conservative; the
// LLM judges which matches are real before proposing a plan.
package main

import (
	"context"
	"strings"
	"text/template"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// auditPromptTemplate is the markdown body returned by the prompt
// handler. The {{.Namespace}} and {{.IncludeReview}} placeholders are
// filled in from the caller's arguments.
const auditPromptTemplate = `You are helping the operator audit a Kubernetes cluster for "agentic"
workloads (LLM-calling services, autonomous agents, MCP servers) that
benefit most from gVisor's sandbox boundary.

Procedure:

1. Call the **scan_cluster** tool. {{if .Namespace}}Restrict the scan to
   namespaces=["{{.Namespace}}"].{{else}}Scan cluster-wide.{{end}}

2. From the returned ScanReport, identify workloads whose container image
   refs match any of the following case-insensitive substrings:

       claude, openai, anthropic, llm, agent, langchain, llamaindex,
       mcp-server, mcp_server, ollama, vllm, autogpt, autogen, crewai,
       semantic-kernel, llama-index, openwebui

   OR whose pod template's env block contains any of these names:

       ANTHROPIC_API_KEY, OPENAI_API_KEY, COHERE_API_KEY, MISTRAL_API_KEY,
       GROQ_API_KEY, GOOGLE_API_KEY, GEMINI_API_KEY, HUGGINGFACE_API_KEY,
       TOGETHER_API_KEY, AZURE_OPENAI_API_KEY

   Treat these heuristics as suggestive, not authoritative. Group matches
   under "likely agentic" and report image and env hits separately.

3. For each likely-agentic workload, call **assess_workload** to get a
   fresh, per-workload Verdict. This gives you the per-rule reasons
   (privileged containers, host network, eBPF, etc.) that the cluster
   scan also reports, but in a form that you can call out individually.

4. Call **propose_plan**.
   {{if .IncludeReview}}Pass include_review=true so "review"-class
   workloads (potentially-degrading features like network-throughput
   or syscall-heavy workloads) are included.{{else}}Leave include_review
   at its default (false). The plan will contain only fully-compatible
   workloads.{{end}}

5. Return a concise summary:
   - which workloads were identified as likely-agentic (by image vs by
     env vs both),
   - which fired blocking compatibility rules (and which rule IDs),
   - which the planner included vs excluded (with reasons),
   - the next concrete step for the operator: typically "apply with
     dry_run=true to preview, then dry_run=false to roll out".

**Do not call apply_plan during the audit.** The audit is read-only.
If the operator wants to migrate, surface the exact tool call they
should run next (including the plan path).`

// auditPromptParams is the (small) Go-side struct that fills in the
// template placeholders. text/template's field-access expects exported
// field names; the JSON-RPC argument names use snake_case via the
// mcp.WithArgument schema below.
type auditPromptParams struct {
	Namespace     string
	IncludeReview bool
}

func registerAuditPrompt(srv *server.MCPServer) {
	tmpl := template.Must(template.New("audit").Parse(auditPromptTemplate))

	prompt := mcp.NewPrompt("audit-cluster-for-agentic-workloads",
		mcp.WithPromptDescription("Heuristically identify likely-agentic Kubernetes workloads (LLM, MCP, agent frameworks) and propose a gVisor migration plan for them. Read-only: never calls apply_plan."),
		mcp.WithArgument("namespace",
			mcp.ArgumentDescription("Optional namespace to restrict the audit to. Empty means cluster-wide."),
		),
		mcp.WithArgument("include_review",
			mcp.ArgumentDescription("Set to 'true' to include 'review'-class workloads in the proposed plan. Default false."),
		),
	)

	srv.AddPrompt(prompt, func(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		args := req.Params.Arguments
		params := auditPromptParams{
			Namespace:     args["namespace"],
			IncludeReview: strings.EqualFold(args["include_review"], "true"),
		}
		var buf strings.Builder
		if err := tmpl.Execute(&buf, params); err != nil {
			return nil, err
		}
		return mcp.NewGetPromptResult(
			"Audit a Kubernetes cluster for likely-agentic workloads and propose a gVisor migration plan.",
			[]mcp.PromptMessage{
				mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent(buf.String())),
			},
		), nil
	})
}
