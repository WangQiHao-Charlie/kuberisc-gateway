package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	unstructured "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Config controls Gateway behavior.
type Config struct {
	Namespace        string
	InstructionSet   string
	Instruction      string
	Runtime          string // exec|grpc
	AgentPodSelector string // label selector to find capable agent pods
	WatchGrace       time.Duration
	Logger           *log.Logger
}

type Server struct {
	cfg       Config
	dyn       dynamic.Interface
	clientset *kubernetes.Clientset
	naRes     dynamic.NamespaceableResourceInterface
}

var (
	naGVR = schema.GroupVersionResource{Group: "risc.dev", Version: "v1alpha1", Resource: "nodeactions"}
)

func NewServer(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	rcfg, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to local kubeconfig for dev; ignore if not present
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	dyn, err := dynamic.NewForConfig(rcfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic: %w", err)
	}
	cs, err := kubernetes.NewForConfig(rcfg)
	if err != nil {
		return nil, fmt.Errorf("clientset: %w", err)
	}
	return &Server{cfg: cfg, dyn: dyn, clientset: cs, naRes: dyn.Resource(naGVR)}, nil
}

// ServeHTTP handles routes under /v1/functions/:name/invoke
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only POST /v1/functions/:name/invoke
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Parse function name
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/functions/"), "/")
	if len(parts) < 2 || parts[1] != "invoke" || parts[0] == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	fnName := parts[0]

	// Query params
	q := r.URL.Query()
	syncQ := q.Get("sync")
	if syncQ == "" || strings.EqualFold(syncQ, "true") {
		// ok
	} else {
		http.Error(w, "only sync=true supported", http.StatusNotImplemented)
		return
	}
	// Parse timeout; default 5s
	timeoutStr := q.Get("timeout")
	if timeoutStr == "" {
		timeoutStr = "5s"
	}
	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil || timeout <= 0 {
		http.Error(w, "invalid timeout", http.StatusBadRequest)
		return
	}

	// Extract X-Request-ID for idempotency
	reqID := r.Header.Get("X-Request-ID")

	// Body as payload
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // limit 1MB MVP
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// Optional module/entrypoint passed via query or headers for MVP
	module := firstNonEmpty(q.Get("module"), r.Header.Get("X-Module"))
	entrypoint := firstNonEmpty(q.Get("entrypoint"), r.Header.Get("X-Entrypoint"))
	if module == "" {
		// In a full setup, module/entrypoint would be derived from a Function. For MVP, require module.
		http.Error(w, "missing module (query param or X-Module)", http.StatusBadRequest)
		return
	}

	// Idempotency: if X-Request-ID provided, try to reuse finished NodeAction
	if reqID != "" {
		if resp, ok, status := s.tryReuseByRequestID(r.Context(), reqID); ok {
			writeHTTP(w, status, resp)
			return
		}
	}

	// Select a capable agent pod and derive nodeName + subject-id
	pod, err := s.pickAgentPod(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("select node: %v", err), http.StatusBadGateway)
		return
	}
	nodeName := pod.Spec.NodeName
	subjectID := pod.Annotations["kuberisc.io/subject-id"]

	// Build NodeAction
	payloadStr := string(body)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	na := map[string]any{
		"apiVersion": "risc.dev/v1alpha1",
		"kind":       "NodeAction",
		"metadata": map[string]any{
			"namespace":    s.cfg.Namespace,
			"generateName": fmt.Sprintf("gw-%s-", sanitize(fnName)),
			"labels": map[string]any{
				"kuberisc.io/gateway":  "true",
				"kuberisc.io/function": fnName,
				"risc.dev/node":        nodeName,
			},
			"annotations": map[string]any{
				"kuberisc.io/created-at": now,
			},
		},
		"spec": map[string]any{
			"nodeName": nodeName,
			"instructionRef": map[string]any{
				"name":        s.cfg.InstructionSet,
				"instruction": s.cfg.Instruction,
				"runtime":     s.cfg.Runtime,
			},
			"resolvedSubjectID": subjectID,
			"timeoutSeconds":    int32(timeout / time.Second),
			"executionID":       nonEmpty(reqID, newExecID()),
			"params": map[string]any{
				"module":     module,
				"entrypoint": entrypoint,
				"payload":    payloadStr,
				"timeout":    timeout.String(), // driver override if needed
			},
		},
	}
	if reqID != "" {
		md := na["metadata"].(map[string]any)
		ann := md["annotations"].(map[string]any)
		ann["kuberisc.io/x-request-id"] = reqID
	}

	// Create
	created, err := s.naRes.Namespace(s.cfg.Namespace).Create(r.Context(), unstructuredFrom(na), metav1.CreateOptions{})
	if err != nil {
		http.Error(w, fmt.Sprintf("create NodeAction: %v", err), http.StatusBadGateway)
		return
	}
	name := created.GetName()
	uid := string(created.GetUID())

	// Watch until Succeeded/Failed or timeout
	ctx, cancel := context.WithTimeout(r.Context(), timeout+s.cfg.WatchGrace)
	defer cancel()

	status, httpStatus, resp := s.waitForCompletion(ctx, name)

	// Idempotency: annotate with completion and uid for traceability (best-effort)
	_ = s.annotateDone(context.Background(), name, map[string]string{
		"kuberisc.io/gateway-phase":  status,
		"kuberisc.io/gateway-http":   strconv.Itoa(httpStatus),
		"kuberisc.io/gateway-na-uid": uid,
	})

	writeHTTP(w, httpStatus, resp)
}

func writeHTTP(w http.ResponseWriter, status int, resp invokeResponse) {
	w.Header().Set("Content-Type", resp.ContentType)
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

type invokeResponse struct {
	ContentType string
	Body        []byte
}

func (s *Server) tryReuseByRequestID(ctx context.Context, reqID string) (invokeResponse, bool, int) {
	// List NodeActions created by gateway with this request id
	ls, err := s.naRes.Namespace(s.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kuberisc.io/gateway=true",
	})
	if err != nil {
		return invokeResponse{}, false, 0
	}
	for i := range ls.Items {
		ann := ls.Items[i].GetAnnotations()
		if ann["kuberisc.io/x-request-id"] != reqID {
			continue
		}
		// Check phase
			phase, _, _ := nestedString(&ls.Items[i], "status", "phase")
			if phase == "Succeeded" || phase == "Failed" {
				_, _, out := s.extractResult(&ls.Items[i])
				return out.Response, true, out.Status
			}
		}
		return invokeResponse{}, false, 0
	}

func (s *Server) pickAgentPod(ctx context.Context) (*corev1.Pod, error) {
	// For MVP: search across all namespaces for pods matching selector and Ready
	pods, err := s.clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: s.cfg.AgentPodSelector})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var candidates []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if p.Spec.NodeName == "" {
			continue
		}
		if !isReady(p) {
			continue
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return nil, errors.New("no ready agent pod found")
	}
	// Deterministic pick: hash time+rand fallback; MVP use first
	idx := rand.Intn(len(candidates))
	return candidates[idx], nil
}

func isReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (s *Server) waitForCompletion(ctx context.Context, name string) (phase string, httpStatus int, resp invokeResponse) {
	// Try watch by name; fall back to polling when unsupported.
	// Start from current resourceVersion
	cur, err := s.naRes.Namespace(s.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		p, ok, out := s.extractResult(cur)
		if ok {
			return p, out.Status, out.Response
		}
	}

	// Watch by name
	fieldSel := fields.OneTermEqualSelector("metadata.name", name).String()
	rv := ""
	if err == nil && cur != nil {
		rv = cur.GetResourceVersion()
	}
	w, err := s.naRes.Namespace(s.cfg.Namespace).Watch(ctx, metav1.ListOptions{FieldSelector: fieldSel, ResourceVersion: rv})
	if err == nil {
		defer w.Stop()
		for {
			select {
			case <-ctx.Done():
				return "Timeout", http.StatusGatewayTimeout, invokeResponse{ContentType: "text/plain", Body: []byte("timeout")}
			case ev, ok := <-w.ResultChan():
				if !ok {
						// watch closed; fall back to poll
						p2, st2, out2 := s.pollUntil(ctx, name)
						return p2, st2, out2.Response
					}
				if ev.Type == watch.Deleted {
					return "Failed", http.StatusBadGateway, invokeResponse{ContentType: "text/plain", Body: []byte("nodeaction deleted")}
				}
				if u, ok := ev.Object.(*unstructured.Unstructured); ok {
					p, done, out := s.extractResult(u)
					if done {
						return p, out.Status, out.Response
					}
				}
			}
		}
	}
	// Fallback: poll
	p3, st3, out3 := s.pollUntil(ctx, name)
	return p3, st3, out3.Response
}

func (s *Server) pollUntil(ctx context.Context, name string) (phase string, status int, out invokeResult) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "Timeout", http.StatusGatewayTimeout, invokeResult{Status: http.StatusGatewayTimeout, Response: invokeResponse{ContentType: "text/plain", Body: []byte("timeout")}}
		case <-ticker.C:
			u, err := s.naRes.Namespace(s.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return "Failed", http.StatusBadGateway, invokeResult{Status: http.StatusBadGateway, Response: invokeResponse{ContentType: "text/plain", Body: []byte("get error")}}
			}
				p, done, out := s.extractResult(u)
				if done {
					return p, out.Status, out
				}
		}
	}
}

type invokeResult struct {
	Status   int
	Response invokeResponse
}

// extractResult maps NodeAction.status into HTTP status & response.
func (s *Server) extractResult(u *unstructured.Unstructured) (phase string, done bool, out invokeResult) {
	phase, _, _ = nestedString(u, "status", "phase")
	if phase == "" || phase == "Pending" || phase == "Running" {
		return phase, false, invokeResult{}
	}
	// Collect fields
	stdout := nestedStringOr(u, "", "status", "result", "stdoutTail")
	stderr := nestedStringOr(u, "", "status", "result", "stderrTail")
	reason := firstConditionReason(u)
	// Map to HTTP status
	switch strings.ToLower(phase) {
	case "succeeded":
		return phase, true, invokeResult{Status: http.StatusOK, Response: invokeResponse{ContentType: contentType(stdout), Body: []byte(stdout)}}
	case "failed":
		if strings.EqualFold(reason, "Timeout") {
			return phase, true, invokeResult{Status: http.StatusGatewayTimeout, Response: invokeResponse{ContentType: "text/plain", Body: []byte("timeout")}}
		}
		// Driver/exec error or non-zero exit
		msg := stderr
		if msg == "" {
			msg = stdout
		}
		if msg == "" {
			msg = reason
		}
		return phase, true, invokeResult{Status: http.StatusBadGateway, Response: invokeResponse{ContentType: contentType(msg), Body: []byte(msg)}}
	default:
		// Treat unknown as 502
		return phase, true, invokeResult{Status: http.StatusBadGateway, Response: invokeResponse{ContentType: "text/plain", Body: []byte("unknown phase")}}
	}
}

func (s *Server) annotateDone(ctx context.Context, name string, kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	// Patch metadata.annotations
	patch := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{},
		},
	}
	for k, v := range kv {
		_ = setNested(patch, v, "metadata", "annotations", k)
	}
	b, _ := json.Marshal(patch)
	_, err := s.naRes.Namespace(s.cfg.Namespace).Patch(ctx, name, types.MergePatchType, b, metav1.PatchOptions{})
	return err
}

// --- helpers: unstructured access ---

// For creating objects
func unstructuredFrom(m map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: m}
}

// Implement runtime.Object minimal methods for client-go dynamic compatibility via type assertion avoidance.

// nested helpers
func nestedString(u *unstructured.Unstructured, fields ...string) (string, bool, error) {
	cur := u.Object
	for i, f := range fields {
		if i == len(fields)-1 {
			if v, ok := cur[f]; ok {
				if s, ok := v.(string); ok {
					return s, true, nil
				}
			}
			return "", false, nil
		}
		v, ok := cur[f]
		if !ok {
			return "", false, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur = m
	}
	return "", false, nil
}

func nestedStringOr(u *unstructured.Unstructured, def string, fields ...string) string {
	s, ok, _ := nestedString(u, fields...)
	if !ok {
		return def
	}
	return s
}

func nestedMapStringString(u *unstructured.Unstructured, fields ...string) (map[string]string, bool, error) {
	cur := u.Object
	for i, f := range fields {
		if i == len(fields)-1 {
			if v, ok := cur[f]; ok {
				if m, ok := v.(map[string]any); ok {
					out := make(map[string]string, len(m))
					for k, vv := range m {
						if s, ok := vv.(string); ok {
							out[k] = s
						}
					}
					return out, true, nil
				}
			}
			return map[string]string{}, false, nil
		}
		v, ok := cur[f]
		if !ok {
			return map[string]string{}, false, nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			return map[string]string{}, false, nil
		}
		cur = m
	}
	return map[string]string{}, false, nil
}

func setNested(dst map[string]any, val any, fields ...string) error {
	cur := dst
	for i, f := range fields {
		if i == len(fields)-1 {
			cur[f] = val
			return nil
		}
		v, ok := cur[f]
		if !ok {
			m := map[string]any{}
			cur[f] = m
			cur = m
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			m = map[string]any{}
			cur[f] = m
		}
		cur = m
	}
	return nil
}

func firstConditionReason(u *unstructured.Unstructured) string {
	// status.conditions is []object with keys type/status/reason
	cur := u.Object
	s, ok := cur["status"].(map[string]any)
	if !ok {
		return ""
	}
	arr, ok := s["conditions"].([]any)
	if !ok {
		return ""
	}
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			if t, _ := m["type"].(string); t == "Executed" {
				if r, _ := m["reason"].(string); r != "" {
					return r
				}
			}
		}
	}
	return ""
}

func contentType(s string) string {
	// Heuristic: if JSON-like, return application/json
	ss := strings.TrimSpace(s)
	if (strings.HasPrefix(ss, "{") && strings.HasSuffix(ss, "}")) || (strings.HasPrefix(ss, "[") && strings.HasSuffix(ss, "]")) {
		return "application/json"
	}
	return "text/plain"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func newExecID() string {
	// timestamp-rand
	return fmt.Sprintf("gw-%d-%04d", time.Now().UnixNano(), rand.Intn(10000))
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ":", "-")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}
