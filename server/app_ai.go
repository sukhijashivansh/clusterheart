package main

// AI chat for the browser dashboard. Answers questions about one cluster,
// grounded in the agent's latest snapshot and the cluster's open incidents.
// Calls OpenRouter with the user's own key when they paste one in the AI tab
// (kept in their browser, sent per request, never stored here), otherwise with
// the server's OPENROUTER_API_KEY. Same reply format as the desktop AI tab.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// aiRL caps chat requests per user, since they may spend the server's key.
var aiRL = newRateLimiter(20, 10*time.Minute)

// aiFreeModels are tried in order when no model is pinned; individual free
// models are often rate-limited, so we fall through to the next.
var aiFreeModels = []string{
	"nex-agi/nex-n2.5-pro:free",
	"nvidia/nemotron-3-super-120b-a12b:free",
	"google/gemma-4-31b-it:free",
}

const aiSystemPrompt = `You are the Clusterheart assistant for ONE Kubernetes cluster, shown in a web dashboard.

OUTPUT FORMAT (most important rule): reply ONLY with bullet lines. Every line is
    [SIGIL] <one short sentence>
where SIGIL is + (good/healthy), ! (problem/risk) or - (neutral fact or next step).
No paragraphs, headings, intros or summaries. Use backticks for names and commands.
Cluster questions: 4-6 bullets. AWS/EKS infrastructure questions: a thorough checklist.

SCOPE: this cluster (pods, nodes, jobs, workloads, events, incidents, resource usage,
rightsizing, kubectl, Helm, SRE) and AWS/EKS infrastructure around it. Refuse anything
else with exactly one bullet:
    [!] I can only help with Kubernetes and AWS infra for this cluster.

GROUNDING: base cluster facts only on the snapshot below; say when something is not in
it. You cannot run commands or see logs; suggest the kubectl command instead.`

// handleAppClusterChat answers one chat turn: POST /api/app/clusters/{id}/chat
// with {"history":[{"role":"user|assistant","text":"..."}],"apiKey":"","model":""}.
func (a *app) handleAppClusterChat(w http.ResponseWriter, r *http.Request) {
	u, tenant, ok := a.appSREUser(w, r)
	if !ok {
		return
	}
	if !aiRL.allow("ai|" + u.Email) {
		writeJSON(w, 429, map[string]any{"error": "Too many questions. Please wait a few minutes and try again."})
		return
	}
	var body struct {
		History []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"history"`
		APIKey string `json:"apiKey"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || len(body.History) == 0 {
		writeJSON(w, 400, map[string]any{"error": "bad request"})
		return
	}
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		key = os.Getenv("OPENROUTER_API_KEY")
	}
	if key == "" {
		writeJSON(w, 400, map[string]any{"error": "AI isn't set up on this server yet."})
		return
	}

	ctxText, err := a.clusterChatContext(tenant, r.PathValue("id"))
	if err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error()})
		return
	}

	msgs := []map[string]string{{"role": "system", "content": aiSystemPrompt + "\n\n=== CLUSTER SNAPSHOT ===\n" + ctxText}}
	hist := body.History
	if len(hist) > 12 {
		hist = hist[len(hist)-12:]
	}
	for _, t := range hist {
		role := "user"
		if t.Role == "assistant" || t.Role == "model" {
			role = "assistant"
		}
		text := t.Text
		if len(text) > 4000 {
			text = text[:4000]
		}
		msgs = append(msgs, map[string]string{"role": role, "content": text})
	}

	models := aiFreeModels
	if m := strings.TrimSpace(body.Model); m != "" {
		models = []string{m}
	} else if m := os.Getenv("OPENROUTER_MODEL"); m != "" {
		models = []string{m}
	}
	var lastErr error
	for _, m := range models {
		text, retry, err := openRouterChat(r.Context(), key, m, msgs)
		if err == nil {
			writeJSON(w, 200, map[string]any{"text": text, "model": m})
			return
		}
		lastErr = err
		if !retry {
			break
		}
	}
	writeJSON(w, 502, map[string]any{"error": lastErr.Error()})
}

// openRouterChat runs one chat completion. retry reports whether another model
// might succeed (rate limits, busy providers) as opposed to key/credit errors.
func openRouterChat(parent context.Context, key, model string, msgs []map[string]string) (text string, retry bool, err error) {
	reqBody, _ := json.Marshal(map[string]any{"model": model, "messages": msgs, "max_tokens": 1200})
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("HTTP-Referer", "https://clusterheart.com")
	req.Header.Set("X-Title", "Clusterheart")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", true, fmt.Errorf("could not reach the AI provider")
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", true, fmt.Errorf("bad response from the AI provider")
	}
	if out.Error != nil {
		code := fmt.Sprintf("%v", out.Error.Code)
		low := strings.ToLower(out.Error.Message)
		fatal := code == "401" || code == "402" || code == "403" || strings.Contains(low, "key") || strings.Contains(low, "credit")
		return "", !fatal, fmt.Errorf("%s", strings.TrimSpace(out.Error.Message))
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return "", true, fmt.Errorf("the model returned no answer; try again")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), false, nil
}

// clusterChatContext renders the cluster's latest snapshot and open incidents
// as compact text for the system prompt.
func (a *app) clusterChatContext(tenant, id string) (string, error) {
	code, data, err := a.cpDo("GET", "/api/v1/clusters?tenantId="+url.QueryEscape(tenant), nil)
	if err != nil || code != 200 {
		return "", fmt.Errorf("could not load the cluster")
	}
	var list struct {
		Clusters []struct {
			ClusterID   string          `json:"clusterId"`
			Name        string          `json:"name"`
			Environment string          `json:"environment"`
			K8sVersion  string          `json:"k8sVersion"`
			Latest      json.RawMessage `json:"latest"`
		} `json:"clusters"`
	}
	if json.Unmarshal(data, &list) != nil {
		return "", fmt.Errorf("could not load the cluster")
	}
	var b strings.Builder
	found := false
	for _, c := range list.Clusters {
		if c.ClusterID != id {
			continue
		}
		found = true
		fmt.Fprintf(&b, "Cluster %q (env %s, Kubernetes %s)\n", c.Name, orDash(c.Environment), orDash(c.K8sVersion))
		writeSnapshotText(&b, c.Latest)
	}
	if !found {
		return "", fmt.Errorf("cluster not found")
	}
	if code, data, err := a.cpDo("GET", "/api/v1/incidents?status=open&tenantId="+url.QueryEscape(tenant), nil); err == nil && code == 200 {
		var incs struct {
			Incidents []struct {
				ClusterID, Type, Severity, Title, Namespace, Workload string
				Occurrences                                           int
			} `json:"incidents"`
		}
		if json.Unmarshal(data, &incs) == nil {
			b.WriteString("\nOpen incidents:\n")
			n := 0
			for _, i := range incs.Incidents {
				if i.ClusterID == id {
					fmt.Fprintf(&b, "- [%s] %s: %s (%s/%s, seen %dx)\n", i.Severity, i.Type, i.Title, i.Namespace, i.Workload, i.Occurrences)
					n++
				}
			}
			if n == 0 {
				b.WriteString("- none\n")
			}
		}
	}
	return b.String(), nil
}

func writeSnapshotText(b *strings.Builder, raw json.RawMessage) {
	var s struct {
		CollectedAt time.Time `json:"collectedAt"`
		Nodes       struct{ Total, Ready int }
		PodList     []struct {
			Namespace, Name, Phase, Ready, Node, Owner string
			Restarts                                   int
			CPUMilli                                   int64 `json:"cpuMilli"`
			MemMi                                      int64 `json:"memMi"`
			CPUReqMilli                                int64 `json:"cpuReqMilli"`
			MemReqMi                                   int64 `json:"memReqMi"`
		} `json:"podList"`
		NodeList []struct {
			Name                                                 string
			Ready                                                bool
			Pods                                                 int
			CPUAllocMilli, CPUUsageMilli, MemAllocMi, MemUsageMi int64
		} `json:"nodeList"`
		JobList []struct {
			Namespace, Name, Owner, Status, Completions string
			Failed                                      int
		} `json:"jobList"`
		PodProblems []struct{ Type, Namespace, Pod, Container, Message string } `json:"podProblems"`
		Warnings    []struct {
			Reason, Namespace, Object, Message string
			Count                              int
		} `json:"warnings"`
	}
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &s) != nil {
		b.WriteString("No snapshot yet (the agent hasn't reported).\n")
		return
	}
	fmt.Fprintf(b, "Snapshot taken %s. Nodes ready %d/%d.\n", s.CollectedAt.Format(time.RFC3339), s.Nodes.Ready, s.Nodes.Total)
	b.WriteString("\nNodes (name ready pods cpuUsed/alloc memUsed/alloc):\n")
	for _, n := range s.NodeList {
		fmt.Fprintf(b, "- %s ready=%v pods=%d cpu=%dm/%dm mem=%dMi/%dMi\n", n.Name, n.Ready, n.Pods, n.CPUUsageMilli, n.CPUAllocMilli, n.MemUsageMi, n.MemAllocMi)
	}
	b.WriteString("\nPods (ns/name phase ready restarts cpu mem [req] owner node):\n")
	for i, p := range s.PodList {
		if i >= 150 {
			fmt.Fprintf(b, "- ... %d more pods\n", len(s.PodList)-i)
			break
		}
		fmt.Fprintf(b, "- %s/%s %s %s r=%d cpu=%dm mem=%dMi req=%dm/%dMi %s %s\n", p.Namespace, p.Name, p.Phase, p.Ready, p.Restarts, p.CPUMilli, p.MemMi, p.CPUReqMilli, p.MemReqMi, p.Owner, p.Node)
	}
	if len(s.JobList) > 0 {
		b.WriteString("\nJobs:\n")
		for i, j := range s.JobList {
			if i >= 40 {
				break
			}
			fmt.Fprintf(b, "- %s/%s %s %s failed=%d %s\n", j.Namespace, j.Name, j.Status, j.Completions, j.Failed, j.Owner)
		}
	}
	if len(s.PodProblems) > 0 {
		b.WriteString("\nProblems:\n")
		for _, p := range s.PodProblems {
			fmt.Fprintf(b, "- %s %s/%s %s: %s\n", p.Type, p.Namespace, p.Pod, p.Container, p.Message)
		}
	}
	if len(s.Warnings) > 0 {
		b.WriteString("\nRecent warning events:\n")
		for i, e := range s.Warnings {
			if i >= 30 {
				break
			}
			fmt.Fprintf(b, "- %s %s/%s x%d: %s\n", e.Reason, e.Namespace, e.Object, e.Count, e.Message)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
