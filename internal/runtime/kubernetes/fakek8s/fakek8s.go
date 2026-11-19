// Package fakek8s is an in-memory Kubernetes API server for one namespace, enough for the
// Kubernetes runtime: pods with a simulated lifecycle and logs, Secrets, PersistentVolumeClaims
// and NetworkPolicies. Like a namespace under the "restricted" Pod Security Standard, it
// refuses pods that could gain privileges.
package fakek8s

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Result is what a simulated container does.
type Result struct {
	Exit           int
	Stdout, Stderr string
	// Hang runs until the pod's deadline passes or it is deleted.
	Hang bool
	// Waiting keeps the container from starting, with this reason, such as ImagePullBackOff.
	Waiting string
}

// Object is a stored object, as JSON fields.
type Object = map[string]any

// Server is the fake.
type Server struct {
	*httptest.Server
	Token     string
	Namespace string
	// Behavior decides what a pod's container does.
	Behavior func(pod Object) Result

	mu       sync.Mutex
	objects  map[string]map[string]Object // kind -> name -> object
	logs     map[string]*podLog
	uid      int
	Refused  []string
	Requests []string
}

type podLog struct {
	stdout, stderr strings.Builder
	done           chan struct{}
	deadline       time.Time
	start          time.Time
}

// New starts a server.
func New(namespace, token string) *Server {
	s := &Server{Token: token, Namespace: namespace, objects: map[string]map[string]Object{},
		logs: map[string]*podLog{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Objects returns copies of the stored objects of a kind.
func (s *Server) Objects(kind string) map[string]Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Object{}
	for k, v := range s.objects[kind] {
		raw, _ := json.Marshal(v)
		var c Object
		_ = json.Unmarshal(raw, &c)
		out[k] = c
	}
	return out
}

var path = regexp.MustCompile(`^/(api/v1|apis/networking\.k8s\.io/v1)/namespaces/([^/]+)/` +
	`(pods|secrets|persistentvolumeclaims|networkpolicies)(?:/([^/]+))?(/log)?$`)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.Requests = append(s.Requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	m := path.FindStringSubmatch(r.URL.Path)
	if m == nil || m[2] != s.Namespace {
		status(w, http.StatusForbidden, "forbidden: outside the namespace")
		return
	}
	kind, name, log := m[3], m[4], m[5] != ""
	switch {
	case log:
		s.serveLog(w, r, name)
	case r.Method == http.MethodPost && name == "":
		s.create(w, r, kind)
	case r.Method == http.MethodGet && name == "":
		s.list(w, r, kind)
	case r.Method == http.MethodGet:
		s.get(w, kind, name)
	case r.Method == http.MethodPatch:
		s.patch(w, r, kind, name)
	case r.Method == http.MethodDelete:
		s.delete(w, kind, name)
	default:
		status(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func status(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(Object{"kind": "Status", "message": msg})
}

func write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func metaOf(o Object) Object {
	m, _ := o["metadata"].(Object)
	if m == nil {
		m = Object{}
		o["metadata"] = m
	}
	return m
}

func (s *Server) create(w http.ResponseWriter, r *http.Request, kind string) {
	var o Object
	if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
		status(w, http.StatusBadRequest, "bad body")
		return
	}
	md := metaOf(o)
	name, _ := md["name"].(string)
	if kind == "pods" {
		if why := restricted(o); why != "" {
			s.mu.Lock()
			s.Refused = append(s.Refused, why)
			s.mu.Unlock()
			status(w, http.StatusForbidden, "violates PodSecurity \"restricted\": "+why)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objects[kind] == nil {
		s.objects[kind] = map[string]Object{}
	}
	if _, ok := s.objects[kind][name]; ok {
		status(w, http.StatusConflict, fmt.Sprintf("%s %q already exists", kind, name))
		return
	}
	s.uid++
	md["uid"] = fmt.Sprintf("uid-%d", s.uid)
	md["namespace"] = s.Namespace
	if kind == "secrets" {
		// Stored, but never echoed back in full: tests read it through Objects.
		o["data"] = o["stringData"]
	}
	if kind == "pods" {
		o["status"] = Object{"phase": "Pending"}
		s.start(name, o)
	}
	s.objects[kind][name] = o
	write(w, http.StatusCreated, o)
}

// restricted checks what the "restricted" Pod Security Standard requires.
func restricted(o Object) string {
	spec, _ := o["spec"].(Object)
	psc, _ := spec["securityContext"].(Object)
	if psc["runAsNonRoot"] != true {
		return "runAsNonRoot != true"
	}
	if sp, _ := psc["seccompProfile"].(Object); sp["type"] != "RuntimeDefault" {
		return "seccompProfile"
	}
	for _, key := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if spec[key] == true {
			return key
		}
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		if _, host := v.(Object)["hostPath"]; host {
			return "hostPath volumes"
		}
	}
	cs, _ := spec["containers"].([]any)
	inits, _ := spec["initContainers"].([]any)
	for _, c := range append(cs, inits...) {
		sc, _ := c.(Object)["securityContext"].(Object)
		if sc["allowPrivilegeEscalation"] != false || sc["privileged"] == true {
			return "allowPrivilegeEscalation != false"
		}
		caps, _ := sc["capabilities"].(Object)
		drop, _ := caps["drop"].([]any)
		if len(drop) != 1 || drop[0] != "ALL" {
			return "capabilities.drop != [ALL]"
		}
	}
	return ""
}

func deadlineOf(o Object) time.Duration {
	spec, _ := o["spec"].(Object)
	if d, ok := spec["activeDeadlineSeconds"].(float64); ok {
		return time.Duration(d) * time.Second
	}
	return 0
}

// start runs the simulated container. Called with the lock held.
func (s *Server) start(name string, o Object) {
	res := Result{}
	if s.Behavior != nil {
		res = s.Behavior(o)
	}
	l := &podLog{done: make(chan struct{}), start: time.Now()}
	if d := deadlineOf(o); d > 0 {
		l.deadline = l.start.Add(d)
	}
	s.logs[name] = l
	go s.run(name, l, res)
}

func (s *Server) run(name string, l *podLog, res Result) {
	defer close(l.done)
	time.Sleep(10 * time.Millisecond)
	s.mu.Lock()
	p, ok := s.objects["pods"][name]
	if !ok {
		s.mu.Unlock()
		return
	}
	if res.Waiting != "" {
		p["status"] = Object{"phase": "Pending", "containerStatuses": []any{Object{"name": "main",
			"state": Object{"waiting": Object{"reason": res.Waiting}}}}}
		s.mu.Unlock()
		return
	}
	p["status"] = Object{"phase": "Running", "containerStatuses": []any{Object{"name": "main",
		"state": Object{"running": Object{"startedAt": "now"}}}}}
	l.stdout.WriteString(res.Stdout)
	l.stderr.WriteString(res.Stderr)
	s.mu.Unlock()
	for res.Hang {
		time.Sleep(10 * time.Millisecond)
		s.mu.Lock()
		p, ok = s.objects["pods"][name]
		over := !l.deadline.IsZero() && time.Now().After(l.deadline)
		if !ok || over {
			if ok {
				p["status"] = Object{"phase": "Failed", "reason": "DeadlineExceeded"}
			}
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok = s.objects["pods"][name]; ok {
		phase := "Succeeded"
		if res.Exit != 0 {
			phase = "Failed"
		}
		p["status"] = Object{"phase": phase, "containerStatuses": []any{Object{"name": "main",
			"state": Object{"terminated": Object{"exitCode": res.Exit}}}}}
	}
}

func (s *Server) get(w http.ResponseWriter, kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[kind][name]
	if !ok {
		status(w, http.StatusNotFound, kind+" not found")
		return
	}
	write(w, http.StatusOK, o)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, kind string) {
	want := map[string]string{}
	for _, part := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
		if k, v, ok := strings.Cut(part, "="); ok {
			want[k] = v
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []Object{}
	for _, o := range s.objects[kind] {
		labels, _ := metaOf(o)["labels"].(Object)
		match := true
		for k, v := range want {
			match = match && labels[k] == v
		}
		if match {
			items = append(items, o)
		}
	}
	write(w, http.StatusOK, Object{"items": items})
}

func (s *Server) patch(w http.ResponseWriter, r *http.Request, kind, name string) {
	if r.Header.Get("Content-Type") != "application/merge-patch+json" {
		status(w, http.StatusUnsupportedMediaType, "unsupported patch")
		return
	}
	var p Object
	_ = json.NewDecoder(r.Body).Decode(&p)
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[kind][name]
	if !ok {
		status(w, http.StatusNotFound, kind+" not found")
		return
	}
	if spec, ok := p["spec"].(Object); ok && kind == "pods" {
		if d, ok := spec["activeDeadlineSeconds"].(float64); ok {
			if old := deadlineOf(o); old > 0 && time.Duration(d)*time.Second > old {
				status(w, http.StatusUnprocessableEntity, "activeDeadlineSeconds can only decrease")
				return
			}
			o["spec"].(Object)["activeDeadlineSeconds"] = d
			if l := s.logs[name]; l != nil {
				l.deadline = l.start.Add(time.Duration(d) * time.Second)
				if time.Since(l.start) > time.Duration(d)*time.Second {
					l.deadline = time.Now()
				}
			}
		}
	}
	merge(o, p)
	write(w, http.StatusOK, o)
}

func merge(dst, src Object) {
	for k, v := range src {
		if sub, ok := v.(Object); ok {
			if d, ok := dst[k].(Object); ok {
				merge(d, sub)
				continue
			}
		}
		dst[k] = v
	}
}

func (s *Server) delete(w http.ResponseWriter, kind, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[kind][name]; !ok {
		status(w, http.StatusNotFound, kind+" not found")
		return
	}
	delete(s.objects[kind], name)
	if kind == "pods" {
		// Owned Secrets go with their pod, as the garbage collector does.
		for sn, sec := range s.objects["secrets"] {
			refs, _ := metaOf(sec)["ownerReferences"].([]any)
			for _, ref := range refs {
				if ref.(Object)["name"] == name {
					delete(s.objects["secrets"], sn)
				}
			}
		}
	}
	write(w, http.StatusOK, Object{"kind": "Status", "status": "Success"})
}

func (s *Server) serveLog(w http.ResponseWriter, r *http.Request, name string) {
	s.mu.Lock()
	l, ok := s.logs[name]
	p := s.objects["pods"][name]
	s.mu.Unlock()
	if !ok || p == nil {
		status(w, http.StatusNotFound, "pod not found")
		return
	}
	if r.URL.Query().Get("follow") == "true" {
		select {
		case <-l.done:
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	out := l.stdout.String() + l.stderr.String()
	switch r.URL.Query().Get("stream") {
	case "Stdout":
		out = l.stdout.String()
	case "Stderr":
		out = l.stderr.String()
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, out)
}
