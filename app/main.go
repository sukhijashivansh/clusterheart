// kubepulse-ui: HTTP server that exposes Kubernetes cluster state as JSON for
// the static frontend in ./static. Mirrors the data collection in ../main.go
// but serves it over REST instead of rendering a TUI.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// Job mirrors a row of `kubectl get jobs` plus enough detail for the UI to
// colour the status and show why a job failed. Completions is the "1/1"
// succeeded/desired string; Duration is start→completion (or start→now while
// still running).
type Job struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Completions string `json:"completions"`
	Succeeded   int32  `json:"succeeded"`
	Failed      int32  `json:"failed"`
	Active      int32  `json:"active"`
	Status      string `json:"status"`
	Duration    string `json:"duration"`
	Age         string `json:"age"`
	Created     int64  `json:"created"`  // unix seconds; used to pick the latest run per job
	Deadline    int64  `json:"deadline"` // activeDeadlineSeconds — time budget allocated, 0 if unset
	OwnerKind   string `json:"ownerKind"`
	OwnerName   string `json:"ownerName"`
	Message     string `json:"message"`
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

type ServiceCost struct {
	Service string  `json:"service"`
	USD     float64 `json:"usd"`
}

// HPAMetric is one metric an HPA scales on (cpu/memory/…), with its current
// reading and target rendered the way `kubectl get hpa` shows them (e.g.
// "31%" / "80%"). CurrentPct/TargetPct are the numeric utilisation percentages
// for the frontend to colour a gauge; -1 means "not a utilisation metric" or
// "no reading yet" (a freshly-scaled pod hasn't reported metrics).
type HPAMetric struct {
	Name       string `json:"name"`
	Current    string `json:"current"`
	Target     string `json:"target"`
	CurrentPct int32  `json:"currentPct"`
	TargetPct  int32  `json:"targetPct"`
}

// HPACond mirrors an HPA status condition (AbleToScale/ScalingActive/
// ScalingLimited) — the "why isn't it scaling" story lives here.
type HPACond struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// HPA is a HorizontalPodAutoscaler flattened for the UI: the scale target, the
// min/max/current/desired replica counts, the per-metric current-vs-target
// readings, and the status conditions that explain scaling decisions.
type HPA struct {
	Namespace       string      `json:"namespace"`
	Name            string      `json:"name"`
	RefKind         string      `json:"refKind"`
	RefName         string      `json:"refName"`
	MinReplicas     int32       `json:"minReplicas"`
	MaxReplicas     int32       `json:"maxReplicas"`
	CurrentReplicas int32       `json:"currentReplicas"`
	DesiredReplicas int32       `json:"desiredReplicas"`
	Metrics         []HPAMetric `json:"metrics"`
	Conditions      []HPACond   `json:"conditions"`
	Age             string      `json:"age"`
	LastScale       string      `json:"lastScale"`
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

	// hist is the MongoDB-backed usage sampler; nil when Mongo is unreachable.
	hist *history

	// sre proxies to the Autonomous SRE control plane (keeps the admin token
	// server-side). Always non-nil; .configured() is false until env is set.
	sre *sreProxy
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

	// Two ways to reach a cluster:
	//   - Local/desktop: read a kubeconfig file (the laptop's ~/.kube/config).
	//   - In-cluster (the hosted web deploy): no kubeconfig file exists, so fall
	//     back to the pod's ServiceAccount via rest.InClusterConfig(). We clear
	//     configPath in that case so the context-switch / AWS-profile code paths
	//     (which read a kubeconfig file) degrade gracefully instead of erroring.
	var (
		cfg *rest.Config
		err error
	)
	if configPath != "" && fileExists(configPath) {
		cfg, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kubeconfig error: %v\n", err)
			os.Exit(1)
		}
	} else {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "no kubeconfig at %q and not running in a cluster: %v\n", configPath, err)
			os.Exit(1)
		}
		configPath = "" // no file to read contexts / AWS profile from
		fmt.Println("using in-cluster ServiceAccount config")
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
	s.initHistory()
	s.sre = newSREProxy()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/pods", s.handlePods)
	mux.HandleFunc("/api/jobs", s.handleJobs)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/hpas", s.handleHPAs)
	mux.HandleFunc("/api/rightsizing", s.handleRightsizing)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/namespaces", s.handleNamespaces)
	mux.HandleFunc("/api/pod/", s.handlePod)
	mux.HandleFunc("/api/namespace/", s.handleNamespaceOps)
	mux.HandleFunc("/api/costs", s.handleCosts)
	mux.HandleFunc("/api/costs/services", s.handleCostsByService)
	mux.HandleFunc("/api/loadtest", s.handleLoadTest)
	mux.HandleFunc("/api/probe", s.handleProbe)
	mux.HandleFunc("/api/gemini", s.handleGemini)
	mux.HandleFunc("/api/shell", s.handleShell)
	mux.HandleFunc("/api/portforward", s.handlePortForwardCollection)
	mux.HandleFunc("/api/portforward/", s.handlePortForwardItem)
	mux.HandleFunc("/api/owner/", s.handlePatchResources)
	mux.HandleFunc("/api/contexts", s.handleContexts)
	mux.HandleFunc("/api/contexts/switch", s.handleSwitchContext)
	// Autonomous SRE control-plane proxy (incidents + remediation approval queue).
	mux.HandleFunc("/api/sre/config", s.handleSREConfig)
	mux.HandleFunc("/api/sre/incidents", s.handleSREIncidents)
	mux.HandleFunc("/api/sre/proposals", s.handleSREProposals)
	mux.HandleFunc("/api/sre/proposals/", s.handleSREProposalItem)
	mux.HandleFunc("/api/sre/policy", s.handleSREPolicy)
	mux.HandleFunc("/api/sre/policy/pause", s.handleSREPolicyPause)
	mux.HandleFunc("/api/sre/policy/resume", s.handleSREPolicyResume)
	mux.HandleFunc("/api/sre/audit", s.handleSREAudit)

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
		// Embedded files have a zero modtime, so http.FileServer emits no
		// Last-Modified/ETag and the Electron/Chromium renderer caches them
		// indefinitely — a rebuilt binary would still show the old UI. Force
		// no-cache so a relaunch always serves the current frontend.
		mux.Handle("/", noCache(http.FileServer(http.FS(sub))))
	}

	fmt.Printf("clusterheart listening on http://%s\n", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
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

// --- /api/jobs ---
// Lists Jobs across all namespaces (the batch/v1 objects behind
// `kubectl get jobs`), including completed ones that linger until their owning
// CronJob / TTL garbage-collects them. This is the history the pods view can't
// show once the job's pods are gone.
func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	jList, err := s.k8s.BatchV1().Jobs("").List(ctx, metav1.ListOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	out := make([]Job, 0, len(jList.Items))
	for _, j := range jList.Items {
		desired := int32(1)
		if j.Spec.Completions != nil {
			desired = *j.Spec.Completions
		}
		completions := fmt.Sprintf("%d/%d", j.Status.Succeeded, desired)

		// Derive a single status from the job conditions, falling back to the
		// active/succeeded/failed counters when no terminal condition is set yet.
		status, msg := "", ""
		for _, c := range j.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete:
				status = "Complete"
			case batchv1.JobFailed:
				status = "Failed"
			}
			if status != "" {
				if c.Message != "" {
					msg = c.Reason + ": " + firstLine(c.Message)
				} else if c.Reason != "" {
					msg = c.Reason
				}
				break
			}
		}
		if status == "" {
			switch {
			case j.Status.Active > 0:
				status = "Running"
			case j.Status.Succeeded >= desired:
				status = "Complete"
			default:
				status = "Pending"
			}
		}

		// Duration = start → completion, or start → now while still running.
		duration := "-"
		if j.Status.StartTime != nil {
			end := time.Now()
			if j.Status.CompletionTime != nil {
				end = j.Status.CompletionTime.Time
			}
			duration = shortAge(end.Sub(j.Status.StartTime.Time))
		}

		var ownerKind, ownerName string
		for _, ref := range j.OwnerReferences {
			if ref.Controller != nil && *ref.Controller {
				ownerKind, ownerName = ref.Kind, ref.Name
				break
			}
		}

		var deadline int64
		if j.Spec.ActiveDeadlineSeconds != nil {
			deadline = *j.Spec.ActiveDeadlineSeconds
		}

		out = append(out, Job{
			Namespace:   j.Namespace,
			Name:        j.Name,
			Completions: completions,
			Succeeded:   j.Status.Succeeded,
			Failed:      j.Status.Failed,
			Active:      j.Status.Active,
			Status:      status,
			Duration:    duration,
			Age:         shortAge(time.Since(j.CreationTimestamp.Time)),
			Created:     j.CreationTimestamp.Unix(),
			Deadline:    deadline,
			OwnerKind:   ownerKind,
			OwnerName:   ownerName,
			Message:     msg,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, out)
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

// --- /api/hpas ---
// Lists HorizontalPodAutoscalers, optionally scoped to ?ns=<namespace>
// (ns=ALL or empty = all namespaces). Flattens each HPA's spec targets against
// its live status readings so the UI can show "cpu: 31%/80%" the way kubectl
// does, plus the status conditions that explain why it is (or isn't) scaling.
func (s *server) handleHPAs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	ns := r.URL.Query().Get("ns")
	if ns == "ALL" {
		ns = ""
	}
	list, err := s.k8s.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	out := make([]HPA, 0, len(list.Items))
	for _, h := range list.Items {
		hi := HPA{
			Namespace:       h.Namespace,
			Name:            h.Name,
			RefKind:         h.Spec.ScaleTargetRef.Kind,
			RefName:         h.Spec.ScaleTargetRef.Name,
			MaxReplicas:     h.Spec.MaxReplicas,
			CurrentReplicas: h.Status.CurrentReplicas,
			DesiredReplicas: h.Status.DesiredReplicas,
			Age:             shortAge(time.Since(h.CreationTimestamp.Time)),
		}
		if h.Spec.MinReplicas != nil {
			hi.MinReplicas = *h.Spec.MinReplicas
		}
		if h.Status.LastScaleTime != nil {
			hi.LastScale = shortAge(time.Since(h.Status.LastScaleTime.Time))
		}

		// Index the live readings by resource name so we can line each spec
		// target up with its current value.
		curPct := map[corev1.ResourceName]int32{}
		curRaw := map[corev1.ResourceName]string{}
		for _, cm := range h.Status.CurrentMetrics {
			if cm.Resource == nil {
				continue
			}
			if v := cm.Resource.Current.AverageUtilization; v != nil {
				curPct[cm.Resource.Name] = *v
			}
			if v := cm.Resource.Current.AverageValue; v != nil && !v.IsZero() {
				curRaw[cm.Resource.Name] = v.String()
			}
		}

		for _, m := range h.Spec.Metrics {
			// Only resource metrics (cpu/memory) carry a utilisation %; surface
			// anything else by its type name so the row still shows up.
			if m.Resource == nil {
				hi.Metrics = append(hi.Metrics, HPAMetric{
					Name: strings.ToLower(string(m.Type)), CurrentPct: -1, TargetPct: -1,
				})
				continue
			}
			met := HPAMetric{Name: string(m.Resource.Name), CurrentPct: -1, TargetPct: -1}
			if v := m.Resource.Target.AverageUtilization; v != nil {
				met.TargetPct = *v
				met.Target = fmt.Sprintf("%d%%", *v)
			} else if v := m.Resource.Target.AverageValue; v != nil && !v.IsZero() {
				met.Target = v.String()
			}
			if p, ok := curPct[m.Resource.Name]; ok {
				met.CurrentPct = p
				met.Current = fmt.Sprintf("%d%%", p)
			} else if raw, ok := curRaw[m.Resource.Name]; ok {
				met.Current = raw
			} else {
				met.Current = "—"
			}
			hi.Metrics = append(hi.Metrics, met)
		}

		for _, c := range h.Status.Conditions {
			hi.Conditions = append(hi.Conditions, HPACond{
				Type:    string(c.Type),
				Status:  string(c.Status),
				Reason:  c.Reason,
				Message: c.Message,
			})
		}
		out = append(out, hi)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
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

	case "endpoints":
		eps, err := s.discoverEndpoints(ctx, ns, name)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"endpoints": eps})
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

// awsEnvFromKubeContext returns environment overrides (AWS_PROFILE / region)
// pulled from the current kube context's exec-auth block, so `aws` subcommands
// authenticate the same way kubectl does for this cluster. Without this, the
// cost command runs against the default profile — which typically has no
// credentials — and fails with NoCredentials even though the cluster is live.
// Returns nil when there's no kubeconfig file or the context uses no exec auth.
func (s *server) awsEnvFromKubeContext() []string {
	path := s.kubeconfigPath()
	if path == "" {
		return nil
	}
	apiCfg, err := clientcmd.LoadFromFile(path)
	if err != nil || apiCfg == nil {
		return nil
	}
	s.clientsMu.RLock()
	ctxName := s.currentCtx
	s.clientsMu.RUnlock()
	if ctxName == "" {
		ctxName = apiCfg.CurrentContext
	}
	kctx, ok := apiCfg.Contexts[ctxName]
	if !ok {
		return nil
	}
	auth, ok := apiCfg.AuthInfos[kctx.AuthInfo]
	if !ok || auth.Exec == nil {
		return nil
	}
	var out []string
	for _, e := range auth.Exec.Env {
		if e.Name == "AWS_PROFILE" && e.Value != "" {
			out = append(out, "AWS_PROFILE="+e.Value)
		}
	}
	// Fall back to the --region arg only if the resolved profile doesn't carry
	// its own region. Cost Explorer is reachable regardless, but the CLI needs
	// a region configured to make the call.
	for i, a := range auth.Exec.Args {
		if a == "--region" && i+1 < len(auth.Exec.Args) {
			out = append(out, "AWS_REGION="+auth.Exec.Args[i+1])
		}
	}
	return out
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
	// Authenticate the same way the active cluster does (SSO profile from the
	// kube context), so costs load without a separate `aws configure`.
	cmd.Env = append(os.Environ(), s.awsEnvFromKubeContext()...)
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
		} else if strings.Contains(msg, "NoCredentials") || strings.Contains(msg, "Unable to locate credentials") {
			msg = "no AWS credentials for the active cluster's profile — run `aws sso login` (or `aws configure`) for it, then retry"
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

// friendlyAwsErr maps the raw aws CLI failure to an actionable message.
func friendlyAwsErr(stderr string, err error) string {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	if strings.Contains(msg, "executable file not found") {
		return "aws CLI not found on PATH"
	}
	if strings.Contains(msg, "NoCredentials") || strings.Contains(msg, "Unable to locate credentials") {
		return "no AWS credentials for the active cluster's profile — run `aws sso login` (or `aws configure`) for it, then retry"
	}
	return msg
}

// costMonthWindow returns the [start, end) date range for a "YYYY-MM" month.
// A blank/invalid month means the current month. The current (ongoing) month is
// capped at today so we ask Cost Explorer for month-to-date, not the future.
func costMonthWindow(month string) (time.Time, time.Time) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	var start time.Time
	if t, err := time.Parse("2006-01", month); err == nil {
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	end := start.AddDate(0, 1, 0) // first of next month, exclusive
	if end.After(today) {
		end = today // ongoing month → month-to-date
	}
	if !end.After(start) {
		end = start.AddDate(0, 0, 1) // month just started; give CE a 1-day window
	}
	return start, end
}

// --- /api/costs/services?month=YYYY-MM  (AWS Cost Explorer, grouped by SERVICE)
// The spend for one month broken down per AWS service (EKS, EC2, EBS, DynamoDB,
// …) so the FinOps view can show where the money actually goes.
func (s *server) handleCostsByService(w http.ResponseWriter, r *http.Request) {
	start, end := costMonthWindow(r.URL.Query().Get("month"))
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "aws", "ce", "get-cost-and-usage",
		"--time-period", fmt.Sprintf("Start=%s,End=%s",
			start.Format("2006-01-02"), end.Format("2006-01-02")),
		"--granularity", "MONTHLY",
		"--metrics", "UnblendedCost",
		"--group-by", "Type=DIMENSION,Key=SERVICE",
		"--output", "json")
	cmd.Env = append(os.Environ(), s.awsEnvFromKubeContext()...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		writeErr(w, 500, friendlyAwsErr(stderr.String(), err))
		return
	}
	var resp struct {
		ResultsByTime []struct {
			Groups []struct {
				Keys    []string `json:"Keys"`
				Metrics struct {
					UnblendedCost struct {
						Amount string `json:"Amount"`
					} `json:"UnblendedCost"`
				} `json:"Metrics"`
			} `json:"Groups"`
		} `json:"ResultsByTime"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		writeErr(w, 500, "parse error: "+err.Error())
		return
	}
	// Sum each service across the (up to two) monthly buckets in the window.
	totals := map[string]float64{}
	for _, rt := range resp.ResultsByTime {
		for _, g := range rt.Groups {
			name := "unknown"
			if len(g.Keys) > 0 && g.Keys[0] != "" {
				name = g.Keys[0]
			}
			amt, _ := strconv.ParseFloat(g.Metrics.UnblendedCost.Amount, 64)
			totals[name] += amt
		}
	}
	out := make([]ServiceCost, 0, len(totals))
	for name, usd := range totals {
		if usd <= 0.005 {
			continue // drop rounding-dust / $0 services to keep the list tight
		}
		out = append(out, ServiceCost{Service: name, USD: usd})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].USD > out[j].USD })
	writeJSON(w, 200, out)
}

// --- /api/loadtest  (JMeter, non-GUI, local CLI) -------------------------
// POST {url, method, threads, rampSeconds, durationSeconds}. Generates a
// minimal JMeter test plan, runs `jmeter -n`, parses the JTL results, and
// returns aggregate stats + a per-second time series. Load originates from
// this machine, so the target must be reachable locally (a public URL, or a
// cluster Service you've port-forwarded via the app).
func (s *server) handleLoadTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var body struct {
		Namespace       string `json:"namespace"`
		Pod             string `json:"pod"`
		Port            int    `json:"port"`
		Scheme          string `json:"scheme"`
		Path            string `json:"path"`
		Method          string `json:"method"`
		Threads         int    `json:"threads"`
		RampSeconds     int    `json:"rampSeconds"`
		DurationSeconds int    `json:"durationSeconds"`
		Body            string `json:"body"`
		ContentType     string `json:"contentType"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad body: "+err.Error())
		return
	}
	ns := strings.TrimSpace(body.Namespace)
	pod := strings.TrimSpace(body.Pod)
	if ns == "" || pod == "" {
		writeErr(w, 400, "namespace and pod required")
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		writeErr(w, 400, "a valid pod port is required")
		return
	}
	scheme := strings.ToLower(strings.TrimSpace(body.Scheme))
	if scheme != "https" {
		scheme = "http"
	}
	reqPath := strings.TrimSpace(body.Path)
	if reqPath == "" {
		reqPath = "/"
	}
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	method := strings.ToUpper(strings.TrimSpace(body.Method))
	if method == "" {
		method = "GET"
	}
	// Bound the knobs so a stray value can't hammer a target or hang the app.
	threads := clampInt(body.Threads, 1, 500)
	ramp := clampInt(body.RampSeconds, 0, 300)
	duration := clampInt(body.DurationSeconds, 1, 300)

	if _, err := exec.LookPath("jmeter"); err != nil {
		writeErr(w, 400, "JMeter not found on PATH — install it with `brew install jmeter` (needs Java, which is present), then retry")
		return
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		writeErr(w, 400, "kubectl not found on PATH — needed to reach the pod")
		return
	}

	// The load run and its port-forward share one deadline: the test duration
	// plus generous headroom for JVM startup and end-of-test flushing.
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(duration+90)*time.Second)
	defer cancel()

	// Ephemeral port-forward to the pod; JMeter hammers the local relay, torn
	// down as soon as the run finishes. (Load hits this one pod through the
	// tunnel, not the Service — it measures single-pod capacity.)
	local, err := freePort()
	if err != nil {
		writeErr(w, 500, "alloc local port: "+err.Error())
		return
	}
	pf := exec.CommandContext(ctx, "kubectl", "port-forward",
		"--address", "127.0.0.1",
		"-n", ns, pod,
		fmt.Sprintf("%d:%d", local, body.Port))
	if kc := s.kubeconfigPath(); kc != "" {
		pf.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	var pfErr bytes.Buffer
	pf.Stderr = &pfErr
	if err := pf.Start(); err != nil {
		writeErr(w, 500, "port-forward: "+err.Error())
		return
	}
	defer func() {
		if pf.Process != nil {
			_ = pf.Process.Kill()
		}
		_ = pf.Wait()
	}()
	if err := waitLocalPort(ctx, local, 10*time.Second); err != nil {
		msg := strings.TrimSpace(pfErr.String())
		if msg == "" {
			msg = "port-forward did not become ready"
		}
		writeErr(w, 502, "cannot reach pod: "+lastLine(msg))
		return
	}

	host := "127.0.0.1"
	port := strconv.Itoa(local)
	path := reqPath

	dir, err := os.MkdirTemp("", "clusterheart-jmeter-")
	if err != nil {
		writeErr(w, 500, "temp dir: "+err.Error())
		return
	}
	defer os.RemoveAll(dir)

	// Build the optional post-body and header-manager XML fragments.
	postBody := strings.TrimSpace(body.Body)
	contentType := strings.TrimSpace(body.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	hasBody := postBody != "" && (method == "POST" || method == "PUT" || method == "PATCH")

	bodySection := `<elementProp name="HTTPsampler.Arguments" elementType="Arguments" guiclass="HTTPArgumentsPanel" testclass="Arguments" testname="User Defined Variables" enabled="true">
            <collectionProp name="Arguments.arguments"/>
          </elementProp>`
	headerSection := ""
	if hasBody {
		bodySection = `<boolProp name="HTTPSampler.postBodyRaw">true</boolProp>
          <elementProp name="HTTPsampler.Arguments" elementType="Arguments" guiclass="HTTPArgumentsPanel" testclass="Arguments" testname="User Defined Variables" enabled="true">
            <collectionProp name="Arguments.arguments">
              <elementProp name="" elementType="HTTPArgument">
                <boolProp name="HTTPArgument.always_encode">false</boolProp>
                <stringProp name="Argument.value">` + xmlEscape(postBody) + `</stringProp>
                <stringProp name="Argument.metadata">=</stringProp>
              </elementProp>
            </collectionProp>
          </elementProp>`
		headerSection = `<HeaderManager guiclass="HeaderPanel" testclass="HeaderManager" testname="HTTP Headers" enabled="true">
          <collectionProp name="HeaderManager.headers">
            <elementProp name="" elementType="Header">
              <stringProp name="Header.name">Content-Type</stringProp>
              <stringProp name="Header.value">` + xmlEscape(contentType) + `</stringProp>
            </elementProp>
          </collectionProp>
        </HeaderManager>
        <hashTree/>`
	}

	plan := jmxTemplate
	repl := strings.NewReplacer(
		"{{THREADS}}", strconv.Itoa(threads),
		"{{RAMP}}", strconv.Itoa(ramp),
		"{{DURATION}}", strconv.Itoa(duration),
		"{{SCHEME}}", scheme,
		"{{HOST}}", xmlEscape(host),
		"{{PORT}}", port,
		"{{PATH}}", xmlEscape(path),
		"{{METHOD}}", method,
		"{{BODY_SECTION}}", bodySection,
		"{{HEADER_SECTION}}", headerSection,
	)
	plan = repl.Replace(plan)

	jmxPath := filepath.Join(dir, "plan.jmx")
	jtlPath := filepath.Join(dir, "results.jtl")
	if err := os.WriteFile(jmxPath, []byte(plan), 0o600); err != nil {
		writeErr(w, 500, "write plan: "+err.Error())
		return
	}

	cmd := exec.CommandContext(ctx, "jmeter", "-n",
		"-t", jmxPath,
		"-l", jtlPath,
		"-j", filepath.Join(dir, "jmeter.log"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			writeErr(w, 504, "load test timed out")
			return
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		writeErr(w, 500, "jmeter failed: "+lastLine(msg))
		return
	}

	res, err := parseJTL(jtlPath)
	if err != nil {
		writeErr(w, 500, "parse results: "+err.Error())
		return
	}
	res.Target = fmt.Sprintf("%s %s://%s.%s:%d%s", method, scheme, pod, ns, body.Port, path)
	res.Threads = threads
	writeJSON(w, 200, res)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xmlEscapeTo(&b, s)
	return b.String()
}
func xmlEscapeTo(b *bytes.Buffer, s string) error {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	_, err := b.WriteString(r.Replace(s))
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Endpoint is one HTTP(S) address the probe UI can pre-fill — discovered from
// a pod's spec so the user doesn't have to know the port/path by heart.
type Endpoint struct {
	Scheme    string `json:"scheme"`
	Port      int    `json:"port"`
	Path      string `json:"path"`
	Label     string `json:"label"`  // human hint, e.g. "readiness probe", "metrics"
	Source    string `json:"source"` // where it came from
	Container string `json:"container,omitempty"`
}

// discoverEndpoints derives the HTTP endpoints a pod plausibly serves, ordered
// most-useful first: health-check probes (authoritative paths), Prometheus
// scrape annotations, raw container ports, then Services that select the pod.
// Duplicates (same scheme+port+path) are collapsed.
func (s *server) discoverEndpoints(ctx context.Context, ns, name string) ([]Endpoint, error) {
	k8s, _, _ := s.clients()
	pod, err := k8s.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var out []Endpoint
	seen := map[string]bool{}
	add := func(e Endpoint) {
		if e.Port <= 0 || e.Port > 65535 {
			return
		}
		if e.Path == "" {
			e.Path = "/"
		}
		if e.Scheme == "" {
			e.Scheme = "http"
		}
		key := fmt.Sprintf("%s|%d|%s", e.Scheme, e.Port, e.Path)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, e)
	}

	// Named container ports let us resolve probe/service targetPorts given by
	// name rather than number.
	portByName := map[string]int32{}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name != "" {
				portByName[p.Name] = p.ContainerPort
			}
		}
	}
	resolve := func(p intstr.IntOrString) int {
		if p.Type == intstr.String {
			if v, ok := portByName[p.StrVal]; ok {
				return int(v)
			}
			return 0
		}
		return p.IntValue()
	}

	// 1. HTTP health-check probes — the paths the app definitely answers.
	for _, c := range pod.Spec.Containers {
		probes := []struct {
			kind  string
			probe *corev1.Probe
		}{
			{"readiness", c.ReadinessProbe},
			{"liveness", c.LivenessProbe},
			{"startup", c.StartupProbe},
		}
		for _, pr := range probes {
			if pr.probe == nil || pr.probe.HTTPGet == nil {
				continue
			}
			h := pr.probe.HTTPGet
			add(Endpoint{
				Scheme:    strings.ToLower(string(h.Scheme)),
				Port:      resolve(h.Port),
				Path:      h.Path,
				Source:    pr.kind + "Probe",
				Container: c.Name,
				Label:     pr.kind + " probe",
			})
		}
	}

	// 2. Prometheus scrape annotations — the metrics endpoint.
	if ann := pod.Annotations; ann != nil {
		if p := ann["prometheus.io/port"]; p != "" {
			port, _ := strconv.Atoi(p)
			path := ann["prometheus.io/path"]
			if path == "" {
				path = "/metrics"
			}
			scheme := ann["prometheus.io/scheme"]
			add(Endpoint{Scheme: scheme, Port: port, Path: path, Source: "prometheus", Label: "metrics"})
		}
	}

	// 3. Raw container ports at "/" — a catch-all when there's no probe.
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			scheme := "http"
			if p.ContainerPort == 443 || strings.Contains(strings.ToLower(p.Name), "https") {
				scheme = "https"
			}
			label := p.Name
			if label == "" {
				label = fmt.Sprintf("port %d", p.ContainerPort)
			}
			add(Endpoint{Scheme: scheme, Port: int(p.ContainerPort), Path: "/", Source: "containerPort", Container: c.Name, Label: label})
		}
	}

	// 4. Services that select this pod — their targetPorts.
	if svcs, err := k8s.CoreV1().Services(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, sv := range svcs.Items {
			if len(sv.Spec.Selector) == 0 || !selectorMatches(sv.Spec.Selector, pod.Labels) {
				continue
			}
			for _, sp := range sv.Spec.Ports {
				if sp.Protocol != "" && sp.Protocol != corev1.ProtocolTCP {
					continue
				}
				tp := resolve(sp.TargetPort)
				if tp == 0 {
					tp = int(sp.Port)
				}
				scheme := "http"
				if sp.Port == 443 || strings.Contains(strings.ToLower(sp.Name), "https") {
					scheme = "https"
				}
				add(Endpoint{Scheme: scheme, Port: tp, Path: "/", Source: "service", Label: "svc " + sv.Name})
			}
		}
	}

	return out, nil
}

// selectorMatches reports whether every label in a Service selector is present
// with the same value on the pod (equality-based selector semantics).
func selectorMatches(sel, labels map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// --- /api/probe  (connection timing breakdown) ---------------------------
// POST {namespace, pod, port, scheme, path, samples}. Opens an ephemeral
// kubectl port-forward to the pod, then times one or more HTTP requests
// through it with net/http/httptrace — breaking each request into the same
// phases `curl -w` reports: DNS, TCP connect, TLS handshake, server-wait
// (time to first byte), and content transfer. Returns per-sample rows plus
// min/avg/max for each phase, then tears the port-forward down.
//
// Timings run through the port-forward tunnel (a local TCP relay over the API
// server), so TCP-connect is to the local relay while TLS + server-wait carry
// the real round trip to the pod. Read absolute numbers as tunnel-inclusive;
// the relative phase split is what's diagnostic.
type ProbeSample struct {
	DNSms      float64 `json:"dnsMs"`
	ConnectMs  float64 `json:"connectMs"`  // TCP handshake
	TLSms      float64 `json:"tlsMs"`      // TLS handshake (0 for plain http)
	WaitMs     float64 `json:"waitMs"`     // request sent -> first byte (server think + return)
	TransferMs float64 `json:"transferMs"` // first byte -> last byte
	TotalMs    float64 `json:"totalMs"`
	Status     int     `json:"status"`
	Bytes      int64   `json:"bytes"`
	Err        string  `json:"err,omitempty"`
}

// ProbeStat is min/avg/max of one phase across the successful samples.
type ProbeStat struct {
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

type ProbeResult struct {
	Target   string        `json:"target"`
	Proto    string        `json:"proto"`
	OKCount  int           `json:"okCount"`
	Count    int           `json:"count"`
	Samples  []ProbeSample `json:"samples"`
	DNS      ProbeStat     `json:"dns"`
	Connect  ProbeStat     `json:"connect"`
	TLS      ProbeStat     `json:"tls"`
	Wait     ProbeStat     `json:"wait"`
	Transfer ProbeStat     `json:"transfer"`
	Total    ProbeStat     `json:"total"`
}

func (s *server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "POST required")
		return
	}
	var body struct {
		Namespace string `json:"namespace"`
		Pod       string `json:"pod"`
		Port      int    `json:"port"`
		Scheme    string `json:"scheme"`
		Path      string `json:"path"`
		Samples   int    `json:"samples"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad body: "+err.Error())
		return
	}
	ns := strings.TrimSpace(body.Namespace)
	pod := strings.TrimSpace(body.Pod)
	if ns == "" || pod == "" {
		writeErr(w, 400, "namespace and pod required")
		return
	}
	if body.Port <= 0 || body.Port > 65535 {
		writeErr(w, 400, "a valid pod port is required")
		return
	}
	scheme := strings.ToLower(strings.TrimSpace(body.Scheme))
	if scheme != "https" {
		scheme = "http"
	}
	reqPath := strings.TrimSpace(body.Path)
	if reqPath == "" {
		reqPath = "/"
	}
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	samples := clampInt(body.Samples, 1, 20)

	if _, err := exec.LookPath("kubectl"); err != nil {
		writeErr(w, 400, "kubectl not found on PATH — needed to reach the pod")
		return
	}

	// Ephemeral port-forward: a local relay we tear down as soon as we're done.
	local, err := freePort()
	if err != nil {
		writeErr(w, 500, "alloc local port: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	pf := exec.CommandContext(ctx, "kubectl", "port-forward",
		"--address", "127.0.0.1",
		"-n", ns, pod,
		fmt.Sprintf("%d:%d", local, body.Port))
	if kc := s.kubeconfigPath(); kc != "" {
		pf.Env = append(os.Environ(), "KUBECONFIG="+kc)
	}
	var pfErr bytes.Buffer
	pf.Stderr = &pfErr
	if err := pf.Start(); err != nil {
		writeErr(w, 500, "port-forward: "+err.Error())
		return
	}
	defer func() {
		if pf.Process != nil {
			_ = pf.Process.Kill()
		}
		_ = pf.Wait()
	}()

	if err := waitLocalPort(ctx, local, 10*time.Second); err != nil {
		msg := strings.TrimSpace(pfErr.String())
		if msg == "" {
			msg = "port-forward did not become ready"
		}
		writeErr(w, 502, "cannot reach pod: "+lastLine(msg))
		return
	}

	target := fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, local, reqPath)
	res := &ProbeResult{
		Target: fmt.Sprintf("%s://%s.%s:%d%s", scheme, pod, ns, body.Port, reqPath),
		Count:  samples,
	}
	for i := 0; i < samples; i++ {
		sm, proto := probeOnce(ctx, target)
		if proto != "" && res.Proto == "" {
			res.Proto = proto
		}
		res.Samples = append(res.Samples, sm)
	}

	// Aggregate only the samples that actually completed a request.
	var dns, conn, tlsD, wait, xfer, total []float64
	for _, sm := range res.Samples {
		if sm.Err != "" {
			continue
		}
		res.OKCount++
		dns = append(dns, sm.DNSms)
		conn = append(conn, sm.ConnectMs)
		tlsD = append(tlsD, sm.TLSms)
		wait = append(wait, sm.WaitMs)
		xfer = append(xfer, sm.TransferMs)
		total = append(total, sm.TotalMs)
	}
	res.DNS = stat(dns)
	res.Connect = stat(conn)
	res.TLS = stat(tlsD)
	res.Wait = stat(wait)
	res.Transfer = stat(xfer)
	res.Total = stat(total)

	if res.OKCount == 0 {
		msg := "all probes failed"
		if len(res.Samples) > 0 && res.Samples[0].Err != "" {
			msg = res.Samples[0].Err
		}
		writeErr(w, 502, msg)
		return
	}
	writeJSON(w, 200, res)
}

// probeOnce runs one HTTP GET against target with a fresh connection (keep-
// alive disabled so every sample re-times DNS/TCP/TLS) and returns the phase
// breakdown plus the negotiated HTTP proto.
func probeOnce(ctx context.Context, target string) (ProbeSample, string) {
	var sm ProbeSample
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		sm.Err = err.Error()
		return sm, ""
	}

	var dnsStart, connStart, tlsStart, firstByte time.Time
	var dnsDur, connDur, tlsDur time.Duration
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { dnsDur = time.Since(dnsStart) },
		ConnectStart:         func(_, _ string) { connStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { connDur = time.Since(connStart) },
		TLSHandshakeStart:    func() { tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { tlsDur = time.Since(tlsStart) },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	tr := &http.Transport{
		DisableKeepAlives: true,
		// The port-forward relay serves the pod's own cert (or none); we're
		// timing the handshake, not verifying a chain, so skip verification.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		sm.Err = cleanProbeErr(err)
		sm.TotalMs = msSince(start)
		return sm, ""
	}
	proto := resp.Proto
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	total := time.Since(start)

	sm.DNSms = round2(ms(dnsDur))
	sm.ConnectMs = round2(ms(connDur))
	sm.TLSms = round2(ms(tlsDur))
	sm.TotalMs = round2(ms(total))
	sm.Status = resp.StatusCode
	sm.Bytes = n
	if !firstByte.IsZero() {
		ttfb := ms(firstByte.Sub(start))
		// server-wait = time-to-first-byte minus the connection setup phases.
		wait := ttfb - sm.DNSms - sm.ConnectMs - sm.TLSms
		if wait < 0 {
			wait = 0
		}
		sm.WaitMs = round2(wait)
		sm.TransferMs = round2(sm.TotalMs - ttfb)
		if sm.TransferMs < 0 {
			sm.TransferMs = 0
		}
	}
	return sm, proto
}

// waitLocalPort blocks until a TCP connection to 127.0.0.1:port succeeds or the
// timeout / context elapses — i.e. the port-forward relay is accepting.
func waitLocalPort(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for 127.0.0.1:%d", port)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// stat reduces a phase's samples to min/avg/max, all rounded to 0.01ms.
func stat(v []float64) ProbeStat {
	if len(v) == 0 {
		return ProbeStat{}
	}
	min, max, sum := v[0], v[0], 0.0
	for _, x := range v {
		if x < min {
			min = x
		}
		if x > max {
			max = x
		}
		sum += x
	}
	return ProbeStat{Min: round2(min), Avg: round2(sum / float64(len(v))), Max: round2(max)}
}

func ms(d time.Duration) float64  { return float64(d.Microseconds()) / 1000.0 }
func msSince(t time.Time) float64 { return round2(ms(time.Since(t))) }
func round2(f float64) float64    { return math.Round(f*100) / 100 }

// cleanProbeErr strips net/http's verbose `Get "url": ` wrapper so the UI shows
// just the underlying cause (connection refused, EOF, timeout, …).
func cleanProbeErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 && strings.HasPrefix(s, "Get ") {
		return strings.TrimSpace(s[i+2:])
	}
	return s
}

// LoadResult is the aggregate report returned to the UI.
type LoadResult struct {
	Target     string       `json:"target"`
	Threads    int          `json:"threads"`
	Samples    int          `json:"samples"`
	Errors     int          `json:"errors"`
	ErrorPct   float64      `json:"errorPct"`
	Throughput float64      `json:"throughput"` // requests/sec over the wall-clock window
	DurationS  float64      `json:"durationS"`
	AvgMs      float64      `json:"avgMs"`
	MinMs      int64        `json:"minMs"`
	MaxMs      int64        `json:"maxMs"`
	P50Ms      int64        `json:"p50Ms"`
	P90Ms      int64        `json:"p90Ms"`
	P95Ms      int64        `json:"p95Ms"`
	P99Ms      int64        `json:"p99Ms"`
	StdDev     float64      `json:"stdDev"`
	PeakRPS    int          `json:"peakRps"`
	PeakAvgMs  float64      `json:"peakAvgMs"`
	Histogram  []HistBucket `json:"histogram"`
	Series     []LoadBucket `json:"series"`
}

// LoadBucket is one second of the test: how many requests completed, their
// average latency, and how many failed.
type LoadBucket struct {
	T      int64   `json:"t"` // seconds since test start
	Count  int     `json:"count"`
	AvgMs  float64 `json:"avgMs"`
	Errors int     `json:"errors"`
}

// HistBucket is one response-time distribution band (e.g. "100–250ms").
type HistBucket struct {
	Label string  `json:"label"`
	Count int     `json:"count"`
	Pct   float64 `json:"pct"`
}

// parseJTL reads JMeter's default CSV results file in a single pass. Relevant
// columns: timeStamp(ms), elapsed(ms), success(bool).
func parseJTL(path string) (*LoadResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1 // JTL rows vary (failureMessage may be empty/quoted)
	header, err := rd.Read()
	if err != nil {
		return nil, fmt.Errorf("no results — target may be unreachable")
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	iTS, okTS := col["timeStamp"]
	iEl, okEl := col["elapsed"]
	iOk, okOk := col["success"]
	if !okTS || !okEl || !okOk {
		return nil, fmt.Errorf("unexpected results format")
	}

	type sample struct {
		ts, el int64
		ok     bool
	}
	var samples []sample
	var sumAll float64
	errors := 0
	var minTS, maxEnd int64
	for {
		rec, rerr := rd.Read()
		if rerr == io.EOF {
			break
		}
		if rerr != nil || len(rec) <= iEl || len(rec) <= iTS || len(rec) <= iOk {
			continue
		}
		ts, _ := strconv.ParseInt(rec[iTS], 10, 64)
		el, _ := strconv.ParseInt(rec[iEl], 10, 64)
		ok := strings.EqualFold(rec[iOk], "true")
		if len(samples) == 0 || ts < minTS {
			minTS = ts
		}
		if end := ts + el; end > maxEnd {
			maxEnd = end
		}
		samples = append(samples, sample{ts, el, ok})
		sumAll += float64(el)
		if !ok {
			errors++
		}
	}
	n := len(samples)
	if n == 0 {
		return nil, fmt.Errorf("no samples recorded — target may be unreachable")
	}

	res := &LoadResult{
		Samples:  n,
		Errors:   errors,
		ErrorPct: math.Round(float64(errors)/float64(n)*1000) / 10,
		AvgMs:    math.Round(sumAll/float64(n)*10) / 10,
	}

	sorted := make([]int64, n)
	for i, s := range samples {
		sorted[i] = s.el
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	res.MinMs, res.MaxMs = sorted[0], sorted[n-1]
	res.P50Ms = percentile(sorted, 50)
	res.P90Ms = percentile(sorted, 90)
	res.P95Ms = percentile(sorted, 95)
	res.P99Ms = percentile(sorted, 99)

	// Standard deviation over the sorted elapsed-ms slice.
	var sumSq float64
	for _, el := range sorted {
		d := float64(el) - res.AvgMs
		sumSq += d * d
	}
	res.StdDev = math.Round(math.Sqrt(sumSq/float64(n))*10) / 10

	// Response-time distribution histogram (6 bands).
	hBounds := []int64{100, 250, 500, 1000, 2000, 1 << 62}
	hLabels := []string{"<100ms", "100–250", "250–500", "500–1s", "1–2s", ">2s"}
	hist := make([]HistBucket, len(hBounds))
	for i, l := range hLabels {
		hist[i].Label = l
	}
	for _, el := range sorted {
		for i, bound := range hBounds {
			if el < bound {
				hist[i].Count++
				break
			}
		}
	}
	for i := range hist {
		hist[i].Pct = math.Round(float64(hist[i].Count)/float64(n)*1000) / 10
	}
	res.Histogram = hist

	wall := float64(maxEnd-minTS) / 1000.0
	if wall <= 0 {
		wall = 1
	}
	res.DurationS = math.Round(wall*10) / 10
	res.Throughput = math.Round(float64(n)/wall*100) / 100

	// Per-second buckets relative to test start.
	m := map[int64]*LoadBucket{}
	for _, s := range samples {
		sec := (s.ts - minTS) / 1000
		if sec < 0 {
			sec = 0
		}
		b := m[sec]
		if b == nil {
			b = &LoadBucket{T: sec}
			m[sec] = b
		}
		b.Count++
		b.AvgMs += float64(s.el) // sum now; averaged below
		if !s.ok {
			b.Errors++
		}
	}
	series := make([]LoadBucket, 0, len(m))
	for _, b := range m {
		b.AvgMs = math.Round(b.AvgMs/float64(b.Count)*10) / 10
		series = append(series, *b)
	}
	sort.Slice(series, func(i, j int) bool { return series[i].T < series[j].T })
	res.Series = series

	// Peak RPS and peak average latency derived from the per-second series.
	for _, b := range res.Series {
		if b.Count > res.PeakRPS {
			res.PeakRPS = b.Count
		}
		if b.AvgMs > res.PeakAvgMs {
			res.PeakAvgMs = b.AvgMs
		}
	}
	res.PeakAvgMs = math.Round(res.PeakAvgMs*10) / 10

	return res, nil
}

// percentile returns the p-th percentile (nearest-rank) of a sorted slice.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

const jmxTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<jmeterTestPlan version="1.2" properties="5.0" jmeter="5.6">
  <hashTree>
    <TestPlan guiclass="TestPlanGui" testclass="TestPlan" testname="Clusterheart Load Test" enabled="true">
      <boolProp name="TestPlan.functional_mode">false</boolProp>
      <boolProp name="TestPlan.serialize_threadgroups">false</boolProp>
      <elementProp name="TestPlan.user_defined_variables" elementType="Arguments" guiclass="ArgumentsPanel" testclass="Arguments" testname="User Defined Variables" enabled="true">
        <collectionProp name="Arguments.arguments"/>
      </elementProp>
      <stringProp name="TestPlan.user_define_classpath"></stringProp>
    </TestPlan>
    <hashTree>
      <ThreadGroup guiclass="ThreadGroupGui" testclass="ThreadGroup" testname="Threads" enabled="true">
        <stringProp name="ThreadGroup.on_sample_error">continue</stringProp>
        <elementProp name="ThreadGroup.main_controller" elementType="LoopController" guiclass="LoopControlPanel" testclass="LoopController" testname="Loop Controller" enabled="true">
          <boolProp name="LoopController.continue_forever">false</boolProp>
          <intProp name="LoopController.loops">-1</intProp>
        </elementProp>
        <stringProp name="ThreadGroup.num_threads">{{THREADS}}</stringProp>
        <stringProp name="ThreadGroup.ramp_time">{{RAMP}}</stringProp>
        <boolProp name="ThreadGroup.scheduler">true</boolProp>
        <stringProp name="ThreadGroup.duration">{{DURATION}}</stringProp>
        <stringProp name="ThreadGroup.delay">0</stringProp>
      </ThreadGroup>
      <hashTree>
        <HTTPSamplerProxy guiclass="HttpTestSampleGui" testclass="HTTPSamplerProxy" testname="HTTP Request" enabled="true">
          {{BODY_SECTION}}
          <stringProp name="HTTPSampler.domain">{{HOST}}</stringProp>
          <stringProp name="HTTPSampler.port">{{PORT}}</stringProp>
          <stringProp name="HTTPSampler.protocol">{{SCHEME}}</stringProp>
          <stringProp name="HTTPSampler.path">{{PATH}}</stringProp>
          <stringProp name="HTTPSampler.method">{{METHOD}}</stringProp>
          <boolProp name="HTTPSampler.follow_redirects">true</boolProp>
          <boolProp name="HTTPSampler.use_keepalive">true</boolProp>
        </HTTPSamplerProxy>
        <hashTree>
          {{HEADER_SECTION}}
        </hashTree>
      </hashTree>
    </hashTree>
  </hashTree>
</jmeterTestPlan>`

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
		ApiKey    string `json:"apiKey"`
		Model     string `json:"model"`
		MaxTokens int    `json:"maxTokens"`
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
		provider = "openrouter"
	}
	if provider != "openrouter" {
		writeErr(w, 400, "unknown provider: "+provider)
		return
	}

	hist := make([]chatTurn, 0, len(body.History))
	for _, t := range body.History {
		hist = append(hist, chatTurn{Role: t.Role, Text: t.Text})
	}

	text, status, err := s.chatLoop(r.Context(), provider, strings.TrimSpace(body.ApiKey), strings.TrimSpace(body.Model), body.MaxTokens, sys, hist)
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

func callGemini(parent context.Context, apiKey, sys string, history []chatTurn) (string, int, error) {
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if apiKey == "" {
		return "", 400, fmt.Errorf("no Gemini API key — add one via the key field in the AI tab (or set GEMINI_API_KEY)")
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
func callOpenRouter(parent context.Context, apiKey, model string, maxTokens int, sys string, history []chatTurn) (string, int, error) {
	// Prefer a key supplied by the UI (stored client-side); fall back to the env
	// var for headless/dev use.
	if apiKey == "" {
		apiKey = os.Getenv("OPENROUTER_API_KEY")
	}
	if apiKey == "" {
		return "", 400, fmt.Errorf("no OpenRouter API key — add one via the key field in the AI tab (or set OPENROUTER_API_KEY)")
	}
	// Model priority: UI field > OPENROUTER_MODEL env > free defaults so chat
	// works with a $0 balance. Paid models (e.g. anthropic/claude-sonnet-4.5)
	// require OpenRouter credits; the ":free" models don't. When no model is
	// pinned we try several free models in order, since any individual free
	// model is often rate-limited or its upstream provider is momentarily down.
	if model == "" {
		model = os.Getenv("OPENROUTER_MODEL")
	}
	var candidates []string
	if model != "" {
		candidates = []string{model} // explicit choice — respect it, no fallback
	} else {
		candidates = []string{
			"nex-agi/nex-n2.5-pro:free",
			"nvidia/nemotron-3-super-120b-a12b:free",
			"nvidia/nemotron-3-ultra-550b-a55b:free",
			"google/gemma-4-26b-a4b-it:free",
			"google/gemma-4-31b-it:free",
		}
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
			Message  string `json:"message"`
			Code     any    `json:"code"`
			Metadata struct {
				ProviderName string `json:"provider_name"`
				Raw          any    `json:"raw"`
			} `json:"metadata"`
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
	// credit balances tend to fall below the default reservation. Priority:
	// UI-supplied maxTokens > OPENROUTER_MAX_TOKENS env > default. A low value
	// lets near-empty balances still get a (short) reply.
	maxTok := 1500
	if v := os.Getenv("OPENROUTER_MAX_TOKENS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxTok = n
		}
	}
	if maxTokens > 0 {
		maxTok = maxTokens
	}
	// attempt runs one chat-completion against a specific model. The bool return
	// says whether it's worth falling back to another free model (true for
	// rate-limit / provider-down / empty replies; false for auth/credit errors,
	// where a different model with the same key won't help).
	attempt := func(m string) (text string, status int, err error, retryable bool) {
		reqBody, _ := json.Marshal(request{Model: m, Messages: msgs, MaxTokens: maxTok})
		ctx, cancel := context.WithTimeout(parent, 60*time.Second)
		defer cancel()
		hreq, _ := http.NewRequestWithContext(ctx, "POST",
			"https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(reqBody))
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Authorization", "Bearer "+apiKey)
		hreq.Header.Set("HTTP-Referer", "https://github.com/local/kubepulse-ui")
		hreq.Header.Set("X-Title", "kubepulse-ui")

		resp, herr := http.DefaultClient.Do(hreq)
		if herr != nil {
			return "", 502, herr, true
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var pr response
		if jerr := json.Unmarshal(raw, &pr); jerr != nil {
			return "", 502, fmt.Errorf("bad openrouter response: %w", jerr), false
		}
		if pr.Error != nil {
			// Free models frequently bubble up a terse "Provider returned error".
			// Surface the code, provider and raw upstream reason so it's
			// actionable.
			msg := strings.TrimSpace(pr.Error.Message)
			if msg == "" {
				msg = "OpenRouter error"
			}
			var extra []string
			code := fmt.Sprintf("%v", pr.Error.Code)
			if code != "" && code != "<nil>" {
				extra = append(extra, "code "+code)
			}
			if p := pr.Error.Metadata.ProviderName; p != "" {
				extra = append(extra, "provider "+p)
			}
			if pr.Error.Metadata.Raw != nil {
				if rawStr := strings.TrimSpace(fmt.Sprintf("%v", pr.Error.Metadata.Raw)); rawStr != "" && rawStr != "<nil>" && rawStr != "map[]" {
					extra = append(extra, "detail: "+rawStr)
				}
			}
			full := msg
			if len(extra) > 0 {
				full += " (" + strings.Join(extra, "; ") + ")"
			}
			low := strings.ToLower(msg)
			// Auth / credit / bad-key problems won't be fixed by another model.
			nonRetryable := code == "401" || code == "402" || code == "403" ||
				strings.Contains(low, "auth") || strings.Contains(low, "credit") ||
				strings.Contains(low, "api key") || strings.Contains(low, "afford")
			return "", 502, fmt.Errorf("%s", full), !nonRetryable
		}
		if len(pr.Choices) == 0 || strings.TrimSpace(pr.Choices[0].Message.Content) == "" {
			return "", 502, fmt.Errorf("model %q returned no text", m), true
		}
		return strings.TrimSpace(pr.Choices[0].Message.Content), 200, nil, false
	}

	var lastErr error
	var lastStatus int
	for _, m := range candidates {
		text, status, err, retryable := attempt(m)
		if err == nil {
			return text, status, nil
		}
		lastErr, lastStatus = err, status
		// Definitive failure (auth/credit/bad key) — another model won't help.
		if !retryable {
			return "", status, err
		}
		// else: retryable — fall through to the next free model
	}
	// Exhausted the fallback list, every attempt was retryable-but-failed.
	if len(candidates) > 1 {
		return "", lastStatus, fmt.Errorf("all free models are busy right now — last: %v (free models are rate-limited; wait ~30s and retry, or pin a paid model in the model field)", lastErr)
	}
	return "", lastStatus, lastErr
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
func (s *server) chatLoop(parent context.Context, provider, apiKey, model string, maxTokens int, sys string, history []chatTurn) (string, int, error) {
	h := append([]chatTurn(nil), history...)
	for round := 0; round < maxToolRounds; round++ {
		var (
			text   string
			status int
			err    error
		)
		switch provider {
		case "gemini":
			text, status, err = callGemini(parent, apiKey, sys, h)
		case "openrouter":
			text, status, err = callOpenRouter(parent, apiKey, model, maxTokens, sys, h)
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

	// Current logs — a generous, byte-capped tail. These lines are the primary
	// signal the chat model reasons over, so give it real content.
	b.WriteString("\nRecent logs (current container, last ~120 lines):\n")
	b.WriteString(s.fetchPodLogs(ctx, ns, name, false))

	// A restarted pod's crash cause lives in the *previous* container's logs,
	// which the current stream no longer shows — surface them so the model can
	// actually diagnose the failure instead of just seeing a healthy restart.
	if restarts > 0 {
		b.WriteString("\nPrevious container logs (before the last restart):\n")
		b.WriteString(s.fetchPodLogs(ctx, ns, name, true))
	}
	return b.String()
}

// fetchPodLogs streams a byte-capped tail of a pod's logs (optionally the
// previous, pre-restart container) for the chat context. Returns a human-
// readable note on failure rather than an error so the snapshot always renders.
func (s *server) fetchPodLogs(ctx context.Context, ns, name string, previous bool) string {
	tail := int64(120)
	limit := int64(12000)
	opts := &corev1.PodLogOptions{TailLines: &tail, LimitBytes: &limit, Timestamps: true, Previous: previous}
	stream, err := s.k8s.CoreV1().Pods(ns).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "(unable to fetch logs: " + err.Error() + ")\n"
	}
	defer stream.Close()
	var buf bytes.Buffer
	io.Copy(&buf, stream)
	out := strings.TrimRight(buf.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "(no log output)\n"
	}
	return out + "\n"
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
