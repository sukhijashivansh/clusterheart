// kubepulse-ui: HTTP server that exposes Kubernetes cluster state as JSON for
// the static frontend in ./static. Mirrors the data collection in ../main.go
// but serves it over REST instead of rendering a TUI.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/homedir"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
)

//go:embed static
var staticFS embed.FS

type Pod struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Ready      string   `json:"ready"`
	Status     string   `json:"status"`
	Restarts   int32    `json:"restarts"`
	CpuUsage   string   `json:"cpuUsage"`
	MemUsage   string   `json:"memUsage"`
	RawCpu     int64    `json:"rawCpu"`
	RawMem     int64    `json:"rawMem"`
	NodeName   string   `json:"nodeName"`
	PodIP      string   `json:"podIP"`
	IsReady    bool     `json:"isReady"`
	Message    string   `json:"message"`
	Age        string   `json:"age"`
	Containers []string `json:"containers"`
	CpuRequest int64    `json:"cpuRequest"`
	CpuLimit   int64    `json:"cpuLimit"`
	MemRequest int64    `json:"memRequest"`
	MemLimit   int64    `json:"memLimit"`
	// OwnerKind is the workload kind that controls this pod, resolved one hop
	// past ReplicaSet/Job so the UI can distinguish CronJob-spawned pods from
	// standalone Jobs and Deployment pods from bare ReplicaSets. Empty for
	// standalone pods. Currently we only do the extra hop for Job→CronJob
	// since that's all the cronjob filter needs.
	OwnerKind string `json:"ownerKind"`
	OwnerName string `json:"ownerName"`
}

type Stats struct {
	TotalCpuUsage int64 `json:"totalCpuUsage"`
	TotalMemUsage int64 `json:"totalMemUsage"`
	TotalCpuCap   int64 `json:"totalCpuCap"`
	TotalMemCap   int64 `json:"totalMemCap"`
	NodeCount     int   `json:"nodeCount"`
}

type Node struct {
	Name       string `json:"name"`
	Role       string `json:"role"`
	Ready      bool   `json:"ready"`
	Age        string `json:"age"`
	CpuCap     int64  `json:"cpuCap"`
	CpuUsed    int64  `json:"cpuUsed"`
	MemCap     int64  `json:"memCap"`
	MemUsed    int64  `json:"memUsed"`
	EphCap     int64  `json:"ephCap"`
	EphUsed    int64  `json:"ephUsed"`
	EphAvail   int64  `json:"ephAvail"`
	EphHasUsed bool   `json:"ephHasUsed"`
	PodCount   int    `json:"podCount"`
	PodLimit   int    `json:"podLimit"`
}

type CostPoint struct {
	Date string  `json:"date"`
	USD  float64 `json:"usd"`
}

type server struct {
	// clientsMu guards k8s/metrics/restCfg/currentCtx so the context-switch
	// endpoint can atomically swap them while handlers are reading.
	clientsMu  sync.RWMutex
	k8s        *kubernetes.Clientset
	metrics    *metricsv.Clientset
	restCfg    *rest.Config
	kubeconfig string
	currentCtx string

	pfMu sync.Mutex
	pfs  map[string]*pfEntry
}

// clients returns a consistent snapshot of the current kube/metrics clients
// and rest config. Handlers that span multiple calls should pin a local copy
// rather than reading server fields directly, so an in-flight context switch
// doesn't tear them across resources.
func (s *server) clients() (*kubernetes.Clientset, *metricsv.Clientset, *rest.Config) {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return s.k8s, s.metrics, s.restCfg
}

func (s *server) kubeconfigPath() string {
	s.clientsMu.RLock()
	defer s.clientsMu.RUnlock()
	return s.kubeconfig
}

// chatTurn is a single message in the chat conversation. Role is "user" or
// "model" (we keep "model" rather than "assistant" because the frontend
// pipeline was originally Gemini-only — translating to "assistant" happens
// inside the OpenAI-compatible call).
type chatTurn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type pfEntry struct {
	ID        string `json:"id"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	PodPort   int    `json:"podPort"`
	LocalPort int    `json:"localPort"`
	Started   string `json:"started"`
	cmd       *exec.Cmd
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

func main() {
	loadDotEnv(".env")
	loadDotEnv("../.env")

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "path to kubeconfig")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
	}
	addr := flag.String("addr", "127.0.0.1:8088", "listen address")
	flag.Parse()

	configPath := *kubeconfig
	if configPath == "" {
		configPath = os.Getenv("KUBECONFIG")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubeconfig error: %v\n", err)
		os.Exit(1)
	}
	k8sClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubernetes client error: %v\n", err)
		os.Exit(1)
	}
	mClient, err := metricsv.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "metrics client error: %v\n", err)
		os.Exit(1)
	}

	// Resolve the current context name from the loaded kubeconfig (if the file
	// exists and parses). Falls back to empty so the UI still works against
	// in-cluster configs that don't have a named context.
	currentCtx := ""
	if apiCfg, err := clientcmd.LoadFromFile(configPath); err == nil && apiCfg != nil {
		currentCtx = apiCfg.CurrentContext
	}

	s := &server{
		k8s:        k8sClient,
		metrics:    mClient,
		restCfg:    cfg,
		kubeconfig: configPath,
		currentCtx: currentCtx,
		pfs:        map[string]*pfEntry{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/pods", s.handlePods)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/namespaces", s.handleNamespaces)
	mux.HandleFunc("/api/pod/", s.handlePod)
	mux.HandleFunc("/api/namespace/", s.handleNamespaceOps)
	mux.HandleFunc("/api/costs", s.handleCosts)
	mux.HandleFunc("/api/gemini", s.handleGemini)
	mux.HandleFunc("/api/shell", s.handleShell)
	mux.HandleFunc("/api/portforward", s.handlePortForwardCollection)
	mux.HandleFunc("/api/portforward/", s.handlePortForwardItem)
	mux.HandleFunc("/api/owner/", s.handlePatchResources)
	mux.HandleFunc("/api/contexts", s.handleContexts)
	mux.HandleFunc("/api/contexts/switch", s.handleSwitchContext)

	// Dev mode: serve ./static live from disk so frontend edits show up on a
	// browser refresh with no rebuild. Set KUBEPULSE_DEV=1 (optionally
	// KUBEPULSE_DEV=/path/to/static to point elsewhere). Otherwise serve the
	// copy embedded into the binary at build time.
	if dev := os.Getenv("KUBEPULSE_DEV"); dev != "" {
		dir := "static"
		if dev != "1" && dev != "true" {
			dir = dev
		}
		fmt.Printf("dev mode: serving frontend from disk (%s)\n", dir)
		mux.Handle("/", noCache(http.FileServer(http.Dir(dir))))
	} else {
		sub, err := fs.Sub(staticFS, "static")
		if err != nil {
			fmt.Fprintf(os.Stderr, "static fs error: %v\n", err)
			os.Exit(1)
		}
		mux.Handle("/", http.FileServer(http.FS(sub)))
	}

	fmt.Printf("clusterheart listening on http://%s\n", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// noCache disables browser caching so live-edited frontend files always reload.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- /api/pods ---
func (s *server) handlePods(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	pList, err := s.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Build a map of Job → controlling CronJob (if any) so we can mark
	// CronJob-spawned pods without an extra Get per pod. A Job created by a
	// CronJob has the CronJob as its first owner reference with controller=true.
	jobOwner := map[string]metav1.OwnerReference{}
	if jList, err := s.k8s.BatchV1().Jobs("").List(ctx, metav1.ListOptions{}); err == nil && jList != nil {
		for _, j := range jList.Items {
			for _, ref := range j.OwnerReferences {
				if ref.Controller != nil && *ref.Controller {
					jobOwner[j.Namespace+"/"+j.Name] = ref
					break
				}
			}
		}
	}
	usage := map[string]corev1.ResourceList{}
	if mList, err := s.metrics.MetricsV1beta1().PodMetricses("").List(ctx, metav1.ListOptions{}); err == nil && mList != nil {
		for _, i := range mList.Items {
			cT, mT := resource.Quantity{}, resource.Quantity{}
			for _, c := range i.Containers {
				cT.Add(*c.Usage.Cpu())
				mT.Add(*c.Usage.Memory())
			}
			usage[i.Namespace+"/"+i.Name] = corev1.ResourceList{
				corev1.ResourceCPU:    cT,
				corev1.ResourceMemory: mT,
			}
		}
	}

	out := make([]Pod, 0, len(pList.Items))
	for _, p := range pList.Items {
		var restarts int32
		ready := 0
		total := len(p.Status.ContainerStatuses)
		msg := ""
		var containerNames []string
		var cpuReq, cpuLim, memReq, memLim int64
		for _, c := range p.Spec.Containers {
			containerNames = append(containerNames, c.Name)
			if r := c.Resources.Requests; r != nil {
				cpuReq += r.Cpu().MilliValue()
				memReq += r.Memory().Value()
			}
			if l := c.Resources.Limits; l != nil {
				cpuLim += l.Cpu().MilliValue()
				memLim += l.Memory().Value()
			}
		}
		for _, c := range p.Status.ContainerStatuses {
			restarts += c.RestartCount
			if c.Ready {
				ready++
			}
			if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
				msg = c.State.Waiting.Reason
				if c.State.Waiting.Message != "" {
					msg = c.State.Waiting.Reason + ": " + firstLine(c.State.Waiting.Message)
				}
			} else if c.State.Terminated != nil && c.State.Terminated.Reason != "" {
				msg = c.State.Terminated.Reason
				if c.State.Terminated.ExitCode != 0 {
					msg = fmt.Sprintf("%s (%d)", msg, c.State.Terminated.ExitCode)
				}
			}
		}
		// Pending without a container-status reason almost always means the
		// scheduler hasn't placed the pod yet — surface PodScheduled=False from
		// pod conditions (Unschedulable + the "0/N nodes available" detail).
		if msg == "" && p.Status.Phase == corev1.PodPending {
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason != "" {
					msg = cond.Reason
					if cond.Message != "" {
						msg = cond.Reason + ": " + firstLine(cond.Message)
					}
					break
				}
			}
		}
		if msg == "" {
			switch {
			case p.Status.Phase == corev1.PodRunning && ready == total:
				msg = "OK"
			case p.Status.Phase == corev1.PodSucceeded:
				msg = "Completed"
			default:
				msg = string(p.Status.Phase)
			}
		}

		var rawCpu, rawMem int64
		cStr, mStr := "-", "-"
		if u, ok := usage[p.Namespace+"/"+p.Name]; ok {
			rawCpu = u.Cpu().MilliValue()
			rawMem = u.Memory().Value()
			cStr = fmt.Sprintf("%dm", rawCpu)
			mStr = fmt.Sprintf("%dMi", rawMem/(1024*1024))
		}

		isReady := (ready == total && total > 0) || p.Status.Phase == "Succeeded"

		var ownerKind, ownerName string
		for _, ref := range p.OwnerReferences {
			if ref.Controller == nil || !*ref.Controller {
				continue
			}
			ownerKind, ownerName = ref.Kind, ref.Name
			if ref.Kind == "Job" {
				if cron, ok := jobOwner[p.Namespace+"/"+ref.Name]; ok {
					ownerKind, ownerName = cron.Kind, cron.Name
				}
			}
			break
		}

		out = append(out, Pod{
			Namespace:  p.Namespace,
			Name:       p.Name,
			Ready:      fmt.Sprintf("%d/%d", ready, total),
			Status:     string(p.Status.Phase),
			Restarts:   restarts,
			CpuUsage:   cStr,
			MemUsage:   mStr,
			RawCpu:     rawCpu,
			RawMem:     rawMem,
			NodeName:   p.Spec.NodeName,
			PodIP:      p.Status.PodIP,
			IsReady:    isReady,
			Message:    msg,
			Age:        shortAge(time.Since(p.CreationTimestamp.Time)),
			Containers: containerNames,
			CpuRequest: cpuReq,
			CpuLimit:   cpuLim,
			MemRequest: memReq,
			MemLimit:   memLim,
			OwnerKind:  ownerKind,
			OwnerName:  ownerName,
		})
	}
	// Final fallback: if any pod still shows a bare phase name (Pending /
	// Unknown / Failed with no container reason yet — e.g. FailedScheduling,
	// FailedAttachVolume, FailedCreatePodSandBox, image-pull errors before the
	// container status materialises), pull the latest Warning event per pod
	// in one call and use its Reason + Message.
	needEvents := false
	for i := range out {
		if isBarePhaseMsg(out[i].Message) {
			needEvents = true
			break
		}
	}
	if needEvents {
		evCtx, evCancel := context.WithTimeout(ctx, 5*time.Second)
		evs, _ := s.k8s.CoreV1().Events("").List(evCtx, metav1.ListOptions{
			FieldSelector: "type=Warning",
		})
		evCancel()
		latest := map[string]corev1.Event{}
		if evs != nil {
			for _, e := range evs.Items {
				if e.InvolvedObject.Kind != "Pod" {
					continue
				}
				key := e.InvolvedObject.Namespace + "/" + e.InvolvedObject.Name
				cur, ok := latest[key]
				if !ok || e.LastTimestamp.After(cur.LastTimestamp.Time) {
					latest[key] = e
				}
			}
		}
		for i := range out {
			if !isBarePhaseMsg(out[i].Message) {
				continue
			}
			if e, ok := latest[out[i].Namespace+"/"+out[i].Name]; ok && e.Reason != "" {
				if e.Message != "" {
					out[i].Message = e.Reason + ": " + firstLine(e.Message)
				} else {
					out[i].Message = e.Reason
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, out)
}

func isBarePhaseMsg(m string) bool {
	switch m {
	case "Pending", "Unknown", "Failed", "":
		return true
	}
	return false
}

// --- /api/nodes ---
func (s *server) handleNodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	nl, err := s.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	metricsMap := map[string]corev1.ResourceList{}
	if nm, err := s.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{}); err == nil && nm != nil {
		for _, item := range nm.Items {
			metricsMap[item.Name] = item.Usage
		}
	}

	podsPerNode := map[string]int{}
	if pl, err := s.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{}); err == nil {
		for _, p := range pl.Items {
			if p.Spec.NodeName != "" && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				podsPerNode[p.Spec.NodeName]++
			}
		}
	}

	out := make([]Node, 0, len(nl.Items))
	for _, n := range nl.Items {
		ni := Node{
			Name:     n.Name,
			CpuCap:   n.Status.Allocatable.Cpu().MilliValue(),
			MemCap:   n.Status.Allocatable.Memory().Value(),
			EphCap:   n.Status.Allocatable.StorageEphemeral().Value(),
			PodLimit: int(n.Status.Allocatable.Pods().Value()),
			PodCount: podsPerNode[n.Name],
			Age:      shortAge(time.Since(n.CreationTimestamp.Time)),
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				ni.Ready = cond.Status == corev1.ConditionTrue
				break
			}
		}
		for k := range n.Labels {
			if strings.HasPrefix(k, "node-role.kubernetes.io/") {
				if role := strings.TrimPrefix(k, "node-role.kubernetes.io/"); role != "" {
					ni.Role = role
					break
				}
			}
		}
		if ni.Role == "" {
			ni.Role = "worker"
		}
		if u, ok := metricsMap[n.Name]; ok {
			ni.CpuUsed = u.Cpu().MilliValue()
			ni.MemUsed = u.Memory().Value()
		}

		sumCtx, sumCancel := context.WithTimeout(ctx, 2*time.Second)
		raw, err := s.k8s.CoreV1().RESTClient().Get().
			AbsPath(fmt.Sprintf("/api/v1/nodes/%s/proxy/stats/summary", n.Name)).
			DoRaw(sumCtx)
		sumCancel()
		if err == nil {
			var sm struct {
				Node struct {
					Fs struct {
						AvailableBytes int64 `json:"availableBytes"`
						UsedBytes      int64 `json:"usedBytes"`
						CapacityBytes  int64 `json:"capacityBytes"`
					} `json:"fs"`
				} `json:"node"`
			}
			if json.Unmarshal(raw, &sm) == nil && sm.Node.Fs.CapacityBytes > 0 {
				ni.EphUsed = sm.Node.Fs.UsedBytes
				ni.EphAvail = sm.Node.Fs.AvailableBytes
				ni.EphHasUsed = true
			}
		}
		out = append(out, ni)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, out)
}

// --- /api/stats ---
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	nodes, err := s.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var stats Stats
	for _, n := range nodes.Items {
		stats.TotalCpuCap += n.Status.Allocatable.Cpu().MilliValue()
		stats.TotalMemCap += n.Status.Allocatable.Memory().Value()
	}
	if nm, _ := s.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{}); nm != nil {
		for _, m := range nm.Items {
			stats.TotalCpuUsage += m.Usage.Cpu().MilliValue()
			stats.TotalMemUsage += m.Usage.Memory().Value()
		}
	}
	stats.NodeCount = len(nodes.Items)
	writeJSON(w, 200, stats)
}

// --- /api/namespaces ---
func (s *server) handleNamespaces(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	nl, err := s.k8s.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]string, 0, len(nl.Items))
	for _, n := range nl.Items {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	writeJSON(w, 200, out)
}

// --- /api/pod/{ns}/{name}/{action} ---
// actions: logs, yaml, diagnose ; DELETE on /api/pod/{ns}/{name} deletes the pod
func (s *server) handlePod(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/pod/"), "/")
	if len(parts) < 2 {
		writeErr(w, 400, "expected /api/pod/{ns}/{name}[/action]")
		return
	}
	ns, name := parts[0], parts[1]
	action := ""
	if len(parts) >= 3 {
		action = parts[2]
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if r.Method == http.MethodDelete && action == "" {
		if err := s.k8s.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "deleted"})
		return
	}

	switch action {
	case "logs":
		container := r.URL.Query().Get("container")
		tail := int64(200)
		if t := r.URL.Query().Get("tail"); t != "" {
			if n, err := strconv.ParseInt(t, 10, 64); err == nil && n > 0 {
				tail = n
			}
		}
		opts := &corev1.PodLogOptions{TailLines: &tail}
		if container != "" {
			opts.Container = container
		}
		req := s.k8s.CoreV1().Pods(ns).GetLogs(name, opts)
		stream, err := req.Stream(ctx)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer stream.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, stream)

	case "yaml":
		cmd := exec.CommandContext(ctx, "kubectl", "get", "pod", name, "-n", ns, "-o", "yaml")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			writeErr(w, 500, msg)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(stdout.Bytes())

	case "owner":
		s.handlePodOwner(w, r.WithContext(ctx), ns, name)
		return

	case "diagnose":
		pod, err := s.k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		var b strings.Builder
		b.WriteString("[EVENTS]\n")
		evs, _ := s.k8s.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", name),
		})
		warnings := 0
		if evs != nil {
			for _, e := range evs.Items {
				if e.Type == "Warning" {
					fmt.Fprintf(&b, "  * %s: %s\n", e.Reason, e.Message)
					warnings++
				}
			}
		}
		if warnings == 0 {
			b.WriteString("  No critical events.\n")
		}

		b.WriteString("\n[ANALYSIS]\n")
		var restarts int32
		ready, total := 0, len(pod.Status.ContainerStatuses)
		for _, c := range pod.Status.ContainerStatuses {
			restarts += c.RestartCount
			if c.Ready {
				ready++
			}
		}
		if restarts > 5 {
			b.WriteString("  [!] High restarts — app likely crashing on init.\n")
		}
		if pod.Status.Phase == "Pending" {
			b.WriteString("  [!] Pending — check node capacity / PVC.\n")
		}
		if pod.Status.Phase == "Running" && ready != total {
			b.WriteString("  [!] Running but not Ready — readiness probe failing or app still starting.\n")
		}
		if restarts == 0 && pod.Status.Phase == "Running" && ready == total {
			b.WriteString("  No issues detected.\n")
		}

		b.WriteString("\n[LOGS]\n")
		tail := int64(20)
		req := s.k8s.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{TailLines: &tail})
		if stream, err := req.Stream(ctx); err == nil {
			defer stream.Close()
			var buf bytes.Buffer
			io.Copy(&buf, stream)
			b.WriteString(buf.String())
		} else {
			b.WriteString("  (logs unavailable: " + err.Error() + ")\n")
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(b.String()))

	default:
		writeErr(w, 404, "unknown action")
	}
}

// --- /api/namespace/{ns}/cleanse  (DELETE all pods) ---
func (s *server) handleNamespaceOps(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/namespace/"), "/")
	if len(parts) < 2 {
		writeErr(w, 400, "expected /api/namespace/{ns}/cleanse")
		return
	}
	ns, action := parts[0], parts[1]
	if action != "cleanse" {
		writeErr(w, 404, "unknown action")
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, 405, "DELETE required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.k8s.CoreV1().Pods(ns).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "all pods deleted", "namespace": ns})
}

// --- /api/costs  (AWS Cost Explorer) ---
func (s *server) handleCosts(w http.ResponseWriter, r *http.Request) {
	end := time.Now().UTC()
	start := time.Date(end.Year(), end.Month()-1, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "aws", "ce", "get-cost-and-usage",
		"--time-period", fmt.Sprintf("Start=%s,End=%s",
			start.Format("2006-01-02"), end.Format("2006-01-02")),
		"--granularity", "DAILY",
		"--metrics", "UnblendedCost",
		"--output", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if strings.Contains(msg, "executable file not found") {
			msg = "aws CLI not found on PATH"
		}
		writeErr(w, 500, msg)
		return
	}
	var resp struct {
		ResultsByTime []struct {
			TimePeriod struct {
				Start string `json:"Start"`
			} `json:"TimePeriod"`
			Total struct {
				UnblendedCost struct {
					Amount string `json:"Amount"`
				} `json:"UnblendedCost"`
			} `json:"Total"`
		} `json:"ResultsByTime"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		writeErr(w, 500, "parse error: "+err.Error())
		return
	}
	points := make([]CostPoint, 0, len(resp.ResultsByTime))
	for _, r := range resp.ResultsByTime {
		amt, _ := strconv.ParseFloat(r.Total.UnblendedCost.Amount, 64)
		points = append(points, CostPoint{Date: r.TimePeriod.Start, USD: amt})
	}
	writeJSON(w, 200, points)
}

// --- /api/gemini ---
// POST body: {"provider":"gemini|openrouter","namespace":"ns","pod":"name","history":[{"role":"user|model","text":"..."}]}
// Endpoint name is historical — it now routes to either Gemini or OpenRouter
// based on `provider` (default: "gemini"). Response shape is unchanged:
// {"text":"..."} on success, {"error":"..."} on failure.
func (s *server) handleGemini(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		History   []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"history"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad body: "+err.Error())
		return
	}
	if len(body.History) == 0 {
		writeErr(w, 400, "history required")
		return
	}

	ctxCluster, cancelCluster := context.WithTimeout(r.Context(), 10*time.Second)
	clusterCtx := s.buildClusterContext(ctxCluster)
	cancelCluster()

	podCtx := ""
	if body.Pod != "" && body.Namespace != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		podCtx = s.buildPodContext(ctx, body.Namespace, body.Pod)
	}

	sys := buildChatSystemPrompt(clusterCtx, podCtx)

	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if provider == "" {
		provider = "gemini"
	}
	if provider != "gemini" && provider != "openrouter" {
		writeErr(w, 400, "unknown provider: "+provider)
		return
	}

	hist := make([]chatTurn, 0, len(body.History))
	for _, t := range body.History {
		hist = append(hist, chatTurn{Role: t.Role, Text: t.Text})
	}

	text, status, err := s.chatLoop(r.Context(), provider, sys, hist)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"text": text, "provider": provider})
}

func buildChatSystemPrompt(clusterCtx, podCtx string) string {
	sys := "OUTPUT FORMAT — THIS IS THE MOST IMPORTANT RULE. READ IT FIRST." +
		"\nYour reply MUST be ONLY bullet lines. The very FIRST character of your reply MUST be `[`." +
		"\nIf you start with any other character (a capital letter, a word, a heading, a sentence), you have failed." +
		"\nDO NOT write paragraphs. DO NOT write introductory sentences. DO NOT write closing summaries. DO NOT write markdown headings (# ##). DO NOT use ordinary `- ` or `* ` bullets." +
		"\n" +
		"\nEvery bullet has this exact shape:" +
		"\n    [SIGIL] <one short sentence>" +
		"\nwhere SIGIL is exactly one of these three characters:" +
		"\n    +   good / healthy / passing / nothing wrong  (UI renders green)" +
		"\n    !   bad / failing / risk / warning / problem  (UI renders red)" +
		"\n    -   neutral fact / count / suggestion         (UI renders default)" +
		"\n" +
		"\nEXAMPLES (copy this style exactly):" +
		"\n    [-] Cluster: 1 node, 17 pods (9 Running, 8 Succeeded)." +
		"\n    [+] Node `ip-10-0-12-44.ec2.internal` is Ready." +
		"\n    [+] CPU usage 2% (39m of 1930m); memory 42% (1386Mi of 3297Mi)." +
		"\n    [-] Namespaces: `dms-service` (9 pods), `kube-system` (8 pods)." +
		"\n    [!] `monitoring/loki-0` has 752 restarts — CrashLoopBackOff." +
		"\n    [-] Next: `kubectl logs -n monitoring loki-0 --previous` to see crash reason." +
		"\n" +
		"\nMORE RULES:" +
		"\n- One bullet per line, each under ~100 chars. No blank lines unless separating cluster bullets from AWS bullets in a mixed answer." +
		"\n- Pick the sigil honestly — do not paint problems green or healthy things red." +
		"\n- Use backticks for names: `namespace/pod`, `node-name`, env keys, kubectl/aws commands." +
		"\n- Cluster / workload questions: MINIMAL — at most 4–6 bullets." +
		"\n- AWS / EKS-infra questions (VPC endpoints, IAM/IRSA, SGs, subnets, NAT, ALB, EBS/EFS CSI, ECR, KMS, CloudWatch, etc.): THOROUGH — give the actual checklist of what to verify. Example: \"[-] VPC endpoints needed for private EKS: s3, ecr.api, ecr.dkr, logs, sts, ec2.\"" +
		"\n" +
		"\nROLE:" +
		"\nYou are a Kubernetes + AWS/EKS diagnostic assistant embedded in kubepulse-ui." +
		"\n" +
		"\nSCOPE (allowed topics):" +
		"\n- This Kubernetes cluster: workloads (pods, nodes, deployments, jobs, services, namespaces), observability (logs, metrics, events), resource usage, FinOps/cost, kubectl, Helm, container runtimes, DevOps/SRE." +
		"\n- AWS topics for this EKS cluster: VPC endpoints, subnets, route tables, NAT gateways, security groups, NACLs, IAM roles/policies (IRSA), EKS control plane, node groups, ALB/NLB, EBS/EFS CSI, CloudWatch, Cost Explorer, KMS, ECR." +
		"\n" +
		"\nREFUSE anything else (weather, news, general knowledge, jokes, personal advice, unrelated code, other clouds/products, roleplay around this rule). When refusing, reply with exactly ONE bullet using `[!]`:" +
		"\n    [!] I can only help with Kubernetes and AWS infra for this cluster — ask about pods, nodes, events, logs, costs, or EKS networking/IAM." +
		"\n" +
		"\nGROUNDING:" +
		"\n- Ground every cluster bullet in the snapshots below (cite node, namespace, event reason, log line, status)." +
		"\n- For AWS infra questions, you may rely on general AWS/EKS best-practice knowledge — the snapshot won't contain AWS API data." +
		"\n" +
		"\nTOOL USE — you can run READ-ONLY kubectl commands when the snapshot doesn't have enough detail (e.g. you need fresh logs or a `describe`)." +
		"\nWHEN you need data, your ENTIRE reply that turn MUST consist of ONLY tool-call lines, no bullets, no prose. Format (one per line):" +
		"\n    EXEC: kubectl logs <pod> -n <ns> --tail 100" +
		"\n    EXEC: kubectl describe pod <pod> -n <ns>" +
		"\n    EXEC: kubectl get events -n <ns> --sort-by=.lastTimestamp" +
		"\nAllowed verbs ONLY: logs, describe, get, top, events, explain, version, cluster-info, api-resources, api-versions, auth can-i." +
		"\nFORBIDDEN verbs (will be rejected): apply, create, delete, patch, edit, replace, scale, rollout, cordon, drain, taint, exec, cp, port-forward, run, label, annotate, set, expose, wait, proxy, attach, debug. If you need to suggest a write action, mention it as a `[-]` bullet in your final answer instead of EXEC." +
		"\nThe system will execute the commands and reply with `[tool outputs ...]` containing the stdout. THEN, in your next turn, give the user the bullet answer that uses what you saw. Do NOT echo the raw command output back — distill it into bullets with the correct sigil." +
		"\nIf the user's question can already be answered from the snapshots below, skip EXEC and answer directly in bullets." +
		"\nMax 3 tool-use rounds before you must finalize in bullets." +
		"\n" +
		"\nREMEMBER:" +
		"\n- A final answer to the user STARTS with `[`. No exceptions." +
		"\n- A tool-call turn STARTS with `EXEC:`. No bullets that turn."
	if clusterCtx != "" {
		sys += "\n\nCurrent cluster snapshot:\n" + clusterCtx
	}
	if podCtx != "" {
		sys += "\n\nCurrent pod snapshot:\n" + podCtx
	}
	return sys
}

func callGemini(parent context.Context, sys string, history []chatTurn) (string, int, error) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		return "", 400, fmt.Errorf("GEMINI_API_KEY env var not set")
	}
	type part struct {
		Text string `json:"text"`
	}
	type content struct {
		Role  string `json:"role,omitempty"`
		Parts []part `json:"parts"`
	}
	type request struct {
		Contents          []content `json:"contents"`
		SystemInstruction *content  `json:"systemInstruction,omitempty"`
	}
	type response struct {
		Candidates []struct {
			Content content `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	contents := make([]content, 0, len(history))
	for _, t := range history {
		role := "user"
		if t.Role == "model" {
			role = "model"
		}
		contents = append(contents, content{Role: role, Parts: []part{{Text: t.Text}}})
	}
	reqBody, _ := json.Marshal(request{
		Contents:          contents,
		SystemInstruction: &content{Parts: []part{{Text: sys}}},
	})

	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-flash-latest:generateContent"
	hreq, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-goog-api-key", apiKey)
	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return "", 502, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var pr response
	if jerr := json.Unmarshal(raw, &pr); jerr != nil {
		return "", 502, fmt.Errorf("bad gemini response: %w", jerr)
	}
	if pr.Error != nil {
		return "", 502, fmt.Errorf("%s", pr.Error.Message)
	}
	if len(pr.Candidates) == 0 || len(pr.Candidates[0].Content.Parts) == 0 {
		return "", 502, fmt.Errorf("empty response from Gemini")
	}
	var out strings.Builder
	for _, p := range pr.Candidates[0].Content.Parts {
		out.WriteString(p.Text)
	}
	return strings.TrimSpace(out.String()), 200, nil
}

// callOpenRouter calls OpenRouter's OpenAI-compatible chat-completions API.
// The model defaults to anthropic/claude-sonnet-4.5 but can be overridden with
// OPENROUTER_MODEL. We pass the kubepulse system prompt as the first
// "system" message and translate our internal {role,text} history (where
// "model" means the assistant) to OpenAI's {role,content} shape.
func callOpenRouter(parent context.Context, sys string, history []chatTurn) (string, int, error) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		return "", 400, fmt.Errorf("OPENROUTER_API_KEY env var not set")
	}
	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		model = "anthropic/claude-sonnet-4.5"
	}
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type request struct {
		Model     string    `json:"model"`
		Messages  []message `json:"messages"`
		MaxTokens int       `json:"max_tokens,omitempty"`
	}
	type response struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	msgs := make([]message, 0, len(history)+1)
	msgs = append(msgs, message{Role: "system", Content: sys})
	for _, t := range history {
		role := "user"
		if t.Role == "model" || t.Role == "assistant" {
			role = "assistant"
		}
		msgs = append(msgs, message{Role: role, Content: t.Text})
	}
	// Cap output so OpenRouter doesn't reserve the model's full 64K context for
	// generation — bullet replies need ~500–800 tokens and pay-as-you-go
	// credit balances tend to fall below the default reservation. Override via
	// OPENROUTER_MAX_TOKENS if needed.
	maxTok := 1500
	if v := os.Getenv("OPENROUTER_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxTok = n
		}
	}
	reqBody, _ := json.Marshal(request{Model: model, Messages: msgs, MaxTokens: maxTok})

	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	hreq, _ := http.NewRequestWithContext(ctx, "POST",
		"https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(reqBody))
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+apiKey)
	hreq.Header.Set("HTTP-Referer", "https://github.com/local/kubepulse-ui")
	hreq.Header.Set("X-Title", "kubepulse-ui")

	resp, err := http.DefaultClient.Do(hreq)
	if err != nil {
		return "", 502, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var pr response
	if jerr := json.Unmarshal(raw, &pr); jerr != nil {
		return "", 502, fmt.Errorf("bad openrouter response: %w", jerr)
	}
	if pr.Error != nil {
		return "", 502, fmt.Errorf("%s", pr.Error.Message)
	}
	if len(pr.Choices) == 0 || strings.TrimSpace(pr.Choices[0].Message.Content) == "" {
		return "", 502, fmt.Errorf("empty response from OpenRouter")
	}
	return strings.TrimSpace(pr.Choices[0].Message.Content), 200, nil
}

// readOnlyKubectlVerbs is the strict allowlist of kubectl subcommands the
// chat is permitted to execute. Any verb not in this list — apply, delete,
// patch, edit, replace, scale, rollout, cordon, drain, taint, exec, cp,
// port-forward, run, label, annotate, set, expose, wait, proxy, attach,
// auth-reconcile, debug, etc. — is refused outright. `auth` is gated to
// `auth can-i` only.
var readOnlyKubectlVerbs = map[string]bool{
	"logs":          true,
	"describe":      true,
	"get":           true,
	"top":           true,
	"events":        true,
	"explain":       true,
	"version":       true,
	"cluster-info":  true,
	"api-resources": true,
	"api-versions":  true,
}

// runReadOnlyKubectl validates an allowlisted kubectl invocation and runs it.
// The chat loop uses this to satisfy the model's EXEC: tool calls. We parse
// the command with strings.Fields (so shell metachars never reach the shell
// — kubectl is exec'd directly with args), enforce the verb allowlist, and
// cap output so a chatty `kubectl logs` can't blow up the context window.
func (s *server) runReadOnlyKubectl(ctx context.Context, cmdStr string) (string, error) {
	trimmed := strings.TrimSpace(cmdStr)
	trimmed = strings.TrimPrefix(trimmed, "kubectl")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return "", fmt.Errorf("empty kubectl command")
	}
	args := strings.Fields(trimmed)
	if len(args) == 0 {
		return "", fmt.Errorf("empty kubectl command")
	}
	for _, a := range args {
		if strings.ContainsAny(a, "|;&`$<>\n\r") {
			return "", fmt.Errorf("disallowed shell metacharacter in argument: %q", a)
		}
	}
	verb := strings.ToLower(args[0])
	if verb == "auth" {
		if len(args) < 2 || args[1] != "can-i" {
			return "", fmt.Errorf("only `kubectl auth can-i` is permitted")
		}
	} else if !readOnlyKubectlVerbs[verb] {
		return "", fmt.Errorf("verb %q is not permitted — only read-only verbs are allowed (logs, describe, get, top, events, explain, version, cluster-info, api-resources, api-versions, auth can-i)", verb)
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "kubectl", args...)
	if path := s.kubeconfigPath(); path != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+path)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := stdout.String()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		// Return any partial stdout alongside the error so the model can see
		// both — e.g. `kubectl logs` may print some lines then fail.
		if out != "" {
			return out, fmt.Errorf("%s", msg)
		}
		return "", fmt.Errorf("%s", msg)
	}
	const maxBytes = 6000
	if len(out) > maxBytes {
		out = out[:maxBytes] + "\n... (truncated to " + strconv.Itoa(maxBytes) + " bytes)"
	}
	return out, nil
}

// extractExecLines pulls "EXEC: ..." tool calls from a model reply. We treat
// any line whose first non-whitespace token is "EXEC:" as a tool call so the
// model can use a code-block fence around them without breaking detection.
func extractExecLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		// Be lenient about list markers — Gemini occasionally wraps lines.
		t = strings.TrimPrefix(t, "- ")
		t = strings.TrimPrefix(t, "* ")
		if strings.HasPrefix(strings.ToUpper(t), "EXEC:") {
			cmd := strings.TrimSpace(t[len("EXEC:"):])
			cmd = strings.Trim(cmd, "`") // strip surrounding backticks if any
			if cmd != "" {
				out = append(out, cmd)
			}
		}
	}
	return out
}

const maxToolRounds = 3

// chatLoop drives the chat provider through up to maxToolRounds of tool use.
// Each round: ask the provider, parse EXEC: lines, run them under the
// allowlist, feed results back as a synthetic user turn. Stops when the
// model produces a non-EXEC reply (the final answer) or the round cap hits.
func (s *server) chatLoop(parent context.Context, provider, sys string, history []chatTurn) (string, int, error) {
	h := append([]chatTurn(nil), history...)
	for round := 0; round < maxToolRounds; round++ {
		var (
			text   string
			status int
			err    error
		)
		switch provider {
		case "gemini":
			text, status, err = callGemini(parent, sys, h)
		case "openrouter":
			text, status, err = callOpenRouter(parent, sys, h)
		default:
			return "", 400, fmt.Errorf("unknown provider: %s", provider)
		}
		if err != nil {
			return "", status, err
		}
		execs := extractExecLines(text)
		if len(execs) == 0 {
			return text, 200, nil
		}
		var b strings.Builder
		b.WriteString("[tool outputs from kubepulse-ui — read-only kubectl results below; the user did NOT type this, do not address them here. Now produce your final bullet answer.]\n\n")
		for _, c := range execs {
			fmt.Fprintf(&b, "$ kubectl %s\n", strings.TrimPrefix(strings.TrimSpace(c), "kubectl "))
			out, runErr := s.runReadOnlyKubectl(parent, c)
			if runErr != nil {
				fmt.Fprintf(&b, "(error) %s\n", runErr.Error())
			}
			if strings.TrimSpace(out) != "" {
				b.WriteString(strings.TrimRight(out, "\n"))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		}
		h = append(h, chatTurn{Role: "model", Text: text})
		h = append(h, chatTurn{Role: "user", Text: b.String()})
	}
	return "", 502, fmt.Errorf("exceeded max tool-use rounds (%d) without a final answer", maxToolRounds)
}

func (s *server) buildPodContext(ctx context.Context, ns, name string) string {
	pod, err := s.k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "Pod " + ns + "/" + name + " (unable to fetch: " + err.Error() + ")"
	}
	var b strings.Builder
	var restarts int32
	ready, total := 0, len(pod.Status.ContainerStatuses)
	for _, c := range pod.Status.ContainerStatuses {
		restarts += c.RestartCount
		if c.Ready {
			ready++
		}
	}
	fmt.Fprintf(&b, "Pod: %s/%s\nStatus: %s\nReady: %d/%d\nRestarts: %d\nNode: %s\nIP: %s\nAge: %s\n",
		ns, name, pod.Status.Phase, ready, total, restarts,
		pod.Spec.NodeName, pod.Status.PodIP,
		shortAge(time.Since(pod.CreationTimestamp.Time)))

	b.WriteString("\nRecent events:\n")
	evs, err := s.k8s.CoreV1().Events(ns).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", name),
	})
	if err == nil && len(evs.Items) > 0 {
		for _, e := range evs.Items {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", e.Type, e.Reason, strings.TrimSpace(e.Message))
		}
	} else {
		b.WriteString("(none)\n")
	}

	b.WriteString("\nLast log lines:\n")
	tail := int64(50)
	req := s.k8s.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{TailLines: &tail})
	stream, err := req.Stream(ctx)
	if err != nil {
		b.WriteString("(unable to fetch logs: " + err.Error() + ")\n")
	} else {
		defer stream.Close()
		var buf bytes.Buffer
		io.Copy(&buf, stream)
		b.WriteString(buf.String())
	}
	return b.String()
}

func (s *server) buildClusterContext(ctx context.Context) string {
	var b strings.Builder

	nodes, nErr := s.k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	nodeUsage := map[string]corev1.ResourceList{}
	if nm, _ := s.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{}); nm != nil {
		for _, m := range nm.Items {
			nodeUsage[m.Name] = m.Usage
		}
	}
	if nErr == nil {
		var cpuCap, memCap, cpuUse, memUse int64
		ready, notReady := 0, 0
		for _, n := range nodes.Items {
			cpuCap += n.Status.Allocatable.Cpu().MilliValue()
			memCap += n.Status.Allocatable.Memory().Value()
			isReady := false
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					isReady = true
					break
				}
			}
			if isReady {
				ready++
			} else {
				notReady++
			}
			if u, ok := nodeUsage[n.Name]; ok {
				cpuUse += u.Cpu().MilliValue()
				memUse += u.Memory().Value()
			}
		}
		fmt.Fprintf(&b, "Nodes: %d total (%d Ready, %d NotReady)\n", len(nodes.Items), ready, notReady)
		if cpuCap > 0 {
			fmt.Fprintf(&b, "Cluster CPU: %dm / %dm (%.0f%% used)\n", cpuUse, cpuCap, float64(cpuUse)*100/float64(cpuCap))
		}
		if memCap > 0 {
			fmt.Fprintf(&b, "Cluster Memory: %dMi / %dMi (%.0f%% used)\n", memUse/(1024*1024), memCap/(1024*1024), float64(memUse)*100/float64(memCap))
		}
		for _, n := range nodes.Items {
			roles := ""
			for k := range n.Labels {
				if strings.HasPrefix(k, "node-role.kubernetes.io/") {
					if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != "" {
						roles = r
						break
					}
				}
			}
			if roles == "" {
				roles = "worker"
			}
			status := "NotReady"
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					status = "Ready"
					break
				}
			}
			fmt.Fprintf(&b, "- %s [%s] %s\n", n.Name, roles, status)
		}
	} else {
		fmt.Fprintf(&b, "(unable to list nodes: %s)\n", nErr.Error())
	}

	pods, pErr := s.k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if pErr == nil {
		running, pending, failed, succeeded, unknown := 0, 0, 0, 0, 0
		byNs := map[string]int{}
		type problem struct {
			ns, name, reason string
			restarts         int32
		}
		var problems []problem
		for _, p := range pods.Items {
			byNs[p.Namespace]++
			switch p.Status.Phase {
			case corev1.PodRunning:
				running++
			case corev1.PodPending:
				pending++
			case corev1.PodFailed:
				failed++
			case corev1.PodSucceeded:
				succeeded++
			default:
				unknown++
			}
			var restarts int32
			ready, total := 0, len(p.Status.ContainerStatuses)
			reason := ""
			for _, c := range p.Status.ContainerStatuses {
				restarts += c.RestartCount
				if c.Ready {
					ready++
				}
				if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
					reason = c.State.Waiting.Reason
				} else if c.State.Terminated != nil && c.State.Terminated.Reason != "" && c.State.Terminated.ExitCode != 0 {
					reason = c.State.Terminated.Reason
				}
			}
			bad := p.Status.Phase == corev1.PodFailed ||
				p.Status.Phase == corev1.PodPending ||
				(p.Status.Phase == corev1.PodRunning && ready < total) ||
				restarts >= 3 ||
				reason != ""
			if bad && p.Status.Phase != corev1.PodSucceeded {
				if reason == "" {
					reason = string(p.Status.Phase)
				}
				problems = append(problems, problem{ns: p.Namespace, name: p.Name, reason: reason, restarts: restarts})
			}
		}
		fmt.Fprintf(&b, "\nPods: %d total (Running=%d, Pending=%d, Failed=%d, Succeeded=%d, Unknown=%d)\n",
			len(pods.Items), running, pending, failed, succeeded, unknown)

		nsNames := make([]string, 0, len(byNs))
		for k := range byNs {
			nsNames = append(nsNames, k)
		}
		sort.Slice(nsNames, func(i, j int) bool { return byNs[nsNames[i]] > byNs[nsNames[j]] })
		b.WriteString("Namespaces (by pod count): ")
		for i, ns := range nsNames {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", ns, byNs[ns])
			if i >= 14 {
				fmt.Fprintf(&b, ", ... (+%d more)", len(nsNames)-i-1)
				break
			}
		}
		b.WriteString("\n")

		if len(problems) > 0 {
			sort.Slice(problems, func(i, j int) bool { return problems[i].restarts > problems[j].restarts })
			b.WriteString("\nProblem pods:\n")
			max := 20
			if len(problems) < max {
				max = len(problems)
			}
			for _, p := range problems[:max] {
				fmt.Fprintf(&b, "- %s/%s: %s (restarts=%d)\n", p.ns, p.name, p.reason, p.restarts)
			}
			if len(problems) > max {
				fmt.Fprintf(&b, "(+%d more problem pods not shown)\n", len(problems)-max)
			}
		} else {
			b.WriteString("\nNo problem pods detected.\n")
		}
	} else {
		fmt.Fprintf(&b, "(unable to list pods: %s)\n", pErr.Error())
	}

	return b.String()
}

// --- /api/pod/{ns}/{name}/owner ---
// Walks ownerReferences to the controlling workload (Deployment via ReplicaSet,
// or StatefulSet/DaemonSet/Job/CronJob directly) so the UI can target the
// right object when patching resources. Returns the resolved owner + per-
// container current request/limit values.
func (s *server) handlePodOwner(w http.ResponseWriter, r *http.Request, ns, name string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pod, err := s.k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	kind, ownerName, err := s.resolveTopOwner(ctx, ns, pod.OwnerReferences)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	type qty struct {
		CPU    string `json:"cpu,omitempty"`
		Memory string `json:"memory,omitempty"`
	}
	type container struct {
		Name     string `json:"name"`
		Requests qty    `json:"requests"`
		Limits   qty    `json:"limits"`
	}
	out := struct {
		Namespace  string      `json:"namespace"`
		Kind       string      `json:"kind"`
		Name       string      `json:"name"`
		Containers []container `json:"containers"`
	}{Namespace: ns, Kind: kind, Name: ownerName}
	for _, c := range pod.Spec.Containers {
		cc := container{Name: c.Name}
		if r := c.Resources.Requests; r != nil {
			if v, ok := r[corev1.ResourceCPU]; ok {
				cc.Requests.CPU = v.String()
			}
			if v, ok := r[corev1.ResourceMemory]; ok {
				cc.Requests.Memory = v.String()
			}
		}
		if l := c.Resources.Limits; l != nil {
			if v, ok := l[corev1.ResourceCPU]; ok {
				cc.Limits.CPU = v.String()
			}
			if v, ok := l[corev1.ResourceMemory]; ok {
				cc.Limits.Memory = v.String()
			}
		}
		out.Containers = append(out.Containers, cc)
	}
	writeJSON(w, 200, out)
}

func (s *server) resolveTopOwner(ctx context.Context, ns string, refs []metav1.OwnerReference) (kind, name string, err error) {
	if len(refs) == 0 {
		return "", "", fmt.Errorf("pod has no owner (standalone) — cannot patch a controller")
	}
	ref := refs[0]
	switch ref.Kind {
	case "ReplicaSet":
		rs, err := s.k8s.AppsV1().ReplicaSets(ns).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil && len(rs.OwnerReferences) > 0 {
			return rs.OwnerReferences[0].Kind, rs.OwnerReferences[0].Name, nil
		}
		return "ReplicaSet", ref.Name, nil
	default:
		return ref.Kind, ref.Name, nil
	}
}

// --- /api/owner/{ns}/{kind}/{name}/patch ---
// POST body: { container, requests: {cpu, memory}, limits: {cpu, memory}, dryRun }
// When dryRun=true we just echo the kubectl command back without running it
// (so the UI can show "copy command" as a safe-by-default option).
func (s *server) handlePatchResources(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/owner/"), "/")
	if len(parts) < 4 || parts[3] != "patch" {
		writeErr(w, 400, "expected /api/owner/{ns}/{kind}/{name}/patch")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	ns, kind, name := parts[0], parts[1], parts[2]

	var body struct {
		Container string `json:"container"`
		Requests  struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"requests"`
		Limits struct {
			CPU    string `json:"cpu"`
			Memory string `json:"memory"`
		} `json:"limits"`
		DryRun bool `json:"dryRun"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad body: "+err.Error())
		return
	}

	args := []string{"set", "resources", "-n", ns, fmt.Sprintf("%s/%s", strings.ToLower(kind), name)}
	if body.Container != "" {
		args = append(args, "-c", body.Container)
	}
	var req []string
	if body.Requests.CPU != "" {
		req = append(req, "cpu="+body.Requests.CPU)
	}
	if body.Requests.Memory != "" {
		req = append(req, "memory="+body.Requests.Memory)
	}
	if len(req) > 0 {
		args = append(args, "--requests="+strings.Join(req, ","))
	}
	var lim []string
	if body.Limits.CPU != "" {
		lim = append(lim, "cpu="+body.Limits.CPU)
	}
	if body.Limits.Memory != "" {
		lim = append(lim, "memory="+body.Limits.Memory)
	}
	if len(lim) > 0 {
		args = append(args, "--limits="+strings.Join(lim, ","))
	}
	if len(req) == 0 && len(lim) == 0 {
		writeErr(w, 400, "at least one of requests/limits must be set")
		return
	}

	cmdStr := "kubectl " + strings.Join(args, " ")
	if body.DryRun {
		writeJSON(w, 200, map[string]string{"status": "dryrun", "command": cmdStr})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if s.kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+s.kubeconfig)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		writeJSON(w, 500, map[string]string{"error": msg, "command": cmdStr})
		return
	}
	writeJSON(w, 200, map[string]string{
		"status":  "applied",
		"command": cmdStr,
		"output":  strings.TrimSpace(stdout.String()),
	})
}

// --- /api/shell  (WebSocket → pod exec) ---
//
// Client → server frames:
//   text starting with "0:" → stdin bytes (everything after the prefix)
//   text starting with "1:" → resize JSON: {"cols":N,"rows":N}
// Server → client frames:
//   text frames with stdout/stderr bytes
//   on close: text frame "__exit__: <msg>" then ws close.
func (s *server) handleShell(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ns := q.Get("ns")
	pod := q.Get("pod")
	container := q.Get("container")
	if ns == "" || pod == "" {
		http.Error(w, "ns and pod required", 400)
		return
	}

	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	req := s.k8s.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec")
	opts := &corev1.PodExecOptions{
		Command: []string{"/bin/sh", "-c", "bash || sh"},
		Stdin:   true,
		Stdout:  true,
		Stderr:  true,
		TTY:     true,
	}
	if container != "" {
		opts.Container = container
	}
	req.VersionedParams(opts, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.restCfg, "POST", req.URL())
	if err != nil {
		writeWsExit(ws, "executor init: "+err.Error())
		return
	}

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	stdoutR, stdoutW := io.Pipe()
	defer stdoutW.Close()

	sizeCh := make(chan remotecommand.TerminalSize, 4)
	sizeQ := wsSizeQueue{ch: sizeCh}

	// reader: ws → stdin / resize
	doneIn := make(chan struct{})
	go func() {
		defer close(doneIn)
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				stdinW.Close()
				return
			}
			if mt != websocket.TextMessage && mt != websocket.BinaryMessage {
				continue
			}
			if len(data) < 2 {
				continue
			}
			switch data[0] {
			case '0':
				if data[1] == ':' {
					stdinW.Write(data[2:])
				}
			case '1':
				if data[1] == ':' {
					var rs struct{ Cols, Rows uint16 }
					if json.Unmarshal(data[2:], &rs) == nil {
						select {
						case sizeCh <- remotecommand.TerminalSize{Width: rs.Cols, Height: rs.Rows}:
						default:
						}
					}
				}
			}
		}
	}()

	// writer: stdout → ws
	doneOut := make(chan struct{})
	go func() {
		defer close(doneOut)
		buf := make([]byte, 4096)
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				if werr := ws.WriteMessage(websocket.TextMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	execErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             stdinR,
		Stdout:            stdoutW,
		Stderr:            stdoutW,
		Tty:               true,
		TerminalSizeQueue: sizeQ,
	})
	stdoutW.Close()
	stdinW.Close()
	<-doneOut

	if execErr != nil {
		writeWsExit(ws, execErr.Error())
	} else {
		writeWsExit(ws, "session ended")
	}
}

func writeWsExit(ws *websocket.Conn, msg string) {
	_ = ws.WriteMessage(websocket.TextMessage, []byte("__exit__: "+msg))
	_ = ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, msg),
		time.Now().Add(2*time.Second))
}

type wsSizeQueue struct{ ch chan remotecommand.TerminalSize }

func (q wsSizeQueue) Next() *remotecommand.TerminalSize {
	sz, ok := <-q.ch
	if !ok {
		return nil
	}
	return &sz
}

// --- /api/portforward  (collection: GET list, POST start) ---
func (s *server) handlePortForwardCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.pfMu.Lock()
		out := make([]*pfEntry, 0, len(s.pfs))
		for _, e := range s.pfs {
			out = append(out, e)
		}
		s.pfMu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].Started < out[j].Started })
		writeJSON(w, 200, out)

	case http.MethodPost:
		var body struct {
			Namespace string `json:"namespace"`
			Pod       string `json:"pod"`
			PodPort   int    `json:"podPort"`
			LocalPort int    `json:"localPort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, "bad body: "+err.Error())
			return
		}
		if body.Namespace == "" || body.Pod == "" || body.PodPort <= 0 {
			writeErr(w, 400, "namespace, pod, podPort required")
			return
		}
		local := body.LocalPort
		if local <= 0 {
			p, err := freePort()
			if err != nil {
				writeErr(w, 500, "no free port: "+err.Error())
				return
			}
			local = p
		}
		entry, err := s.startPortForward(body.Namespace, body.Pod, body.PodPort, local)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, entry)

	default:
		writeErr(w, 405, "method not allowed")
	}
}

// --- /api/portforward/{id}  (DELETE stop) ---
func (s *server) handlePortForwardItem(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/portforward/")
	if id == "" {
		writeErr(w, 400, "id required")
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, 405, "DELETE required")
		return
	}
	s.pfMu.Lock()
	e, ok := s.pfs[id]
	if ok {
		delete(s.pfs, id)
	}
	s.pfMu.Unlock()
	if !ok {
		writeErr(w, 404, "not found")
		return
	}
	if e.cmd != nil && e.cmd.Process != nil {
		_ = e.cmd.Process.Kill()
	}
	writeJSON(w, 200, map[string]string{"status": "stopped", "id": id})
}

func (s *server) startPortForward(ns, pod string, podPort, localPort int) (*pfEntry, error) {
	cmd := exec.Command("kubectl", "port-forward",
		"--address", "127.0.0.1",
		"-n", ns, pod,
		fmt.Sprintf("%d:%d", localPort, podPort))
	if s.kubeconfig != "" {
		cmd.Env = append(os.Environ(), "KUBECONFIG="+s.kubeconfig)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	id := fmt.Sprintf("pf-%d-%d", time.Now().UnixNano(), localPort)
	entry := &pfEntry{
		ID:        id,
		Namespace: ns,
		Pod:       pod,
		PodPort:   podPort,
		LocalPort: localPort,
		Started:   time.Now().Format(time.RFC3339),
		cmd:       cmd,
	}
	s.pfMu.Lock()
	s.pfs[id] = entry
	s.pfMu.Unlock()

	// reap when the child exits so the map doesn't accumulate dead entries
	go func() {
		_ = cmd.Wait()
		s.pfMu.Lock()
		delete(s.pfs, id)
		s.pfMu.Unlock()
	}()
	return entry, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// firstLine collapses multi-line strings (event messages, scheduler
// explanations) to a single trimmed line and caps length so they fit in the
// Message column.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 140 {
		s = s[:137] + "..."
	}
	return s
}

func shortAge(d time.Duration) string {
	if d.Hours() > 24 {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if d.Hours() > 1 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	if d.Minutes() > 1 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// --- /api/contexts  (GET) ---
// Lists every context in the kubeconfig file plus which one is active. The
// frontend uses this to populate the context-switcher dropdown.
func (s *server) handleContexts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 405, "GET required")
		return
	}
	path := s.kubeconfigPath()
	if path == "" {
		writeJSON(w, 200, map[string]any{"contexts": []string{}, "current": ""})
		return
	}
	apiCfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		writeErr(w, 500, "load kubeconfig: "+err.Error())
		return
	}
	names := make([]string, 0, len(apiCfg.Contexts))
	for name := range apiCfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)

	s.clientsMu.RLock()
	current := s.currentCtx
	s.clientsMu.RUnlock()
	if current == "" {
		current = apiCfg.CurrentContext
	}
	writeJSON(w, 200, map[string]any{
		"contexts": names,
		"current":  current,
	})
}

// --- /api/contexts/switch  (POST) ---
// Body: {"context":"name"}. Rebuilds the kube/metrics clients against the
// chosen context, atomically swaps them in, and persists the change to the
// kubeconfig file (matching `kubectl config use-context` semantics) so the
// switch is durable across restarts.
func (s *server) handleSwitchContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var body struct {
		Context string `json:"context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad body: "+err.Error())
		return
	}
	if body.Context == "" {
		writeErr(w, 400, "context required")
		return
	}
	path := s.kubeconfigPath()
	if path == "" {
		writeErr(w, 400, "no kubeconfig file loaded — cannot switch contexts")
		return
	}
	apiCfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		writeErr(w, 500, "load kubeconfig: "+err.Error())
		return
	}
	if _, ok := apiCfg.Contexts[body.Context]; !ok {
		writeErr(w, 404, "context not found: "+body.Context)
		return
	}

	newCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
		&clientcmd.ConfigOverrides{CurrentContext: body.Context},
	).ClientConfig()
	if err != nil {
		writeErr(w, 500, "build config: "+err.Error())
		return
	}
	newKube, err := kubernetes.NewForConfig(newCfg)
	if err != nil {
		writeErr(w, 500, "kube client: "+err.Error())
		return
	}
	newMetrics, err := metricsv.NewForConfig(newCfg)
	if err != nil {
		writeErr(w, 500, "metrics client: "+err.Error())
		return
	}

	// Persist the switch so subsequent kubectl / kubepulse runs see the same
	// context. If the write fails we still swap in memory but surface the
	// error so the user knows it won't survive a restart.
	apiCfg.CurrentContext = body.Context
	persistErr := clientcmd.WriteToFile(*apiCfg, path)

	s.clientsMu.Lock()
	s.k8s = newKube
	s.metrics = newMetrics
	s.restCfg = newCfg
	s.currentCtx = body.Context
	s.clientsMu.Unlock()

	resp := map[string]any{
		"status":  "switched",
		"current": body.Context,
	}
	if persistErr != nil {
		resp["warning"] = "context switched in memory but failed to persist to kubeconfig: " + persistErr.Error()
	}
	writeJSON(w, 200, resp)
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if _, present := os.LookupEnv(key); !present {
			os.Setenv(key, val)
		}
	}
}
