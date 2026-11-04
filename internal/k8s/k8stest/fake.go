// Package k8stest 提供测试用的假 Kubernetes API：实现 Pod / Secret 的增删查，
// 并以「假 kubelet」在进程内依次执行 init 容器与主容器（卷映射到临时目录，环境变量中的挂载路径相应替换）。
package k8stest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"flow/internal/k8s"
)

// ExecFunc 在进程内执行一个容器，返回退出码与终止消息。getenv 中的挂载路径已映射到本地目录。
type ExecFunc func(ctx context.Context, c k8s.Container, getenv func(string) string) (int, string)

type Fake struct {
	*httptest.Server
	Exec ExecFunc
	// PullErrors 中的镜像在启动时进入等待状态（原因为对应值，如 ImagePullBackOff）。
	PullErrors map[string]string

	root    string
	mu      sync.Mutex
	pods    map[string]*k8s.Pod
	secrets map[string]*k8s.Secret
	cancel  map[string]context.CancelFunc
	created []string
	deleted []string
}

func New(t *testing.T, exec ExecFunc) *Fake {
	f := &Fake{Exec: exec, root: t.TempDir(), PullErrors: map[string]string{},
		pods: map[string]*k8s.Pod{}, secrets: map[string]*k8s.Secret{}, cancel: map[string]context.CancelFunc{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		for _, c := range f.cancel {
			c()
		}
		f.mu.Unlock()
		f.Close()
	})
	return f
}

// Created / Deleted 返回创建与删除过的 Pod 名。
func (f *Fake) Created() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}
func (f *Fake) Deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// Pod 返回 Pod 的当前副本（已删除的返回 nil）。
func (f *Fake) Pod(name string) *k8s.Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.pods[name]; p != nil {
		c := *p
		return &c
	}
	return nil
}

// Secrets 返回现存 Secret 的名字。
func (f *Fake) Secrets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.secrets {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// VolumeDir 返回 Pod 某个卷映射到的本地目录。
func (f *Fake) VolumeDir(pod, volume string) string {
	return filepath.Join(f.root, "pods", pod, volume)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"), "/")
	if len(parts) < 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "not found"})
		return
	}
	kind, name := parts[1], ""
	if len(parts) > 2 {
		name = parts[2]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case kind == "pods" && r.Method == http.MethodPost:
		var p k8s.Pod
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		if f.pods[p.Metadata.Name] != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "pod already exists"})
			return
		}
		p.Metadata.UID = fmt.Sprintf("uid-%s-%d", p.Metadata.Name, len(f.created))
		p.Status = k8s.PodStatus{Phase: "Pending"}
		f.pods[p.Metadata.Name] = &p
		f.created = append(f.created, p.Metadata.Name)
		ctx, cancel := context.WithCancel(context.Background())
		f.cancel[p.Metadata.Name] = cancel
		go f.kubelet(ctx, p)
		writeJSON(w, http.StatusCreated, p)
	case kind == "pods" && r.Method == http.MethodGet && name == "":
		sel := map[string]string{}
		for _, kv := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				sel[k] = v
			}
		}
		items := []k8s.Pod{}
		for _, p := range f.pods {
			match := true
			for k, v := range sel {
				if p.Metadata.Labels[k] != v {
					match = false
				}
			}
			if match {
				items = append(items, *p)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case kind == "pods" && r.Method == http.MethodGet:
		if p := f.pods[name]; p != nil {
			writeJSON(w, http.StatusOK, p)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "pod not found"})
	case kind == "pods" && r.Method == http.MethodDelete:
		p := f.pods[name]
		if p == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "pod not found"})
			return
		}
		f.cancel[name]()
		delete(f.pods, name)
		delete(f.cancel, name)
		f.deleted = append(f.deleted, name)
		for sn, s := range f.secrets { // 模拟按 ownerReference 的级联回收
			for _, o := range s.Metadata.OwnerReferences {
				if o.UID == p.Metadata.UID {
					delete(f.secrets, sn)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "Success"})
	case kind == "secrets" && r.Method == http.MethodPost:
		var s k8s.Secret
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
		f.secrets[s.Metadata.Name] = &s
		writeJSON(w, http.StatusCreated, s)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "unsupported"})
	}
}

func (f *Fake) update(name string, fn func(p *k8s.Pod)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := f.pods[name]; p != nil {
		fn(p)
	}
}

func (f *Fake) secret(name string) *k8s.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.secrets[name]
}

// kubelet 模拟节点：等待 Secret、准备卷，然后依次运行 init 容器与主容器并更新状态。
func (f *Fake) kubelet(ctx context.Context, p k8s.Pod) {
	name := p.Metadata.Name
	for _, c := range append(append([]k8s.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
		if reason := f.PullErrors[c.Image]; reason != "" {
			f.update(name, func(p *k8s.Pod) {
				p.Status.InitContainerStatuses = []k8s.ContainerStatus{{Name: c.Name, State: k8s.ContainerState{
					Waiting: &k8s.StateWaiting{Reason: reason, Message: "Back-off pulling image " + c.Image}}}}
			})
			return
		}
	}
	vols := map[string]string{}
	secrets := map[string]*k8s.Secret{}
	for _, v := range p.Spec.Volumes {
		dir := f.VolumeDir(name, v.Name)
		switch {
		case v.PersistentVolumeClaim != nil:
			dir = filepath.Join(f.root, "pvc", v.PersistentVolumeClaim.ClaimName)
		case v.Secret != nil:
			var s *k8s.Secret
			for deadline := time.Now().Add(5 * time.Second); s == nil; {
				if s = f.secret(v.Secret.SecretName); s == nil {
					if time.Now().After(deadline) || ctx.Err() != nil {
						return // 与真实 kubelet 一样一直 Pending
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			secrets[v.Secret.SecretName] = s
			os.MkdirAll(dir, 0o755)
			for k, val := range s.StringData {
				os.WriteFile(filepath.Join(dir, k), []byte(val), 0o444)
			}
		}
		os.MkdirAll(dir, 0o755)
		vols[v.Name] = dir
	}
	run := func(c k8s.Container) (int, string) {
		mounts := append([]k8s.VolumeMount(nil), c.VolumeMounts...)
		sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].MountPath) > len(mounts[j].MountPath) })
		mapPath := func(v string) string {
			for _, m := range mounts {
				if v == m.MountPath || strings.HasPrefix(v, m.MountPath+"/") {
					return vols[m.Name] + strings.TrimPrefix(v, m.MountPath)
				}
			}
			return v
		}
		env := map[string]string{}
		for _, e := range c.Env {
			v := e.Value
			if ref := e.ValueFrom; ref != nil && ref.SecretKeyRef != nil {
				if s := secrets[ref.SecretKeyRef.Name]; s != nil {
					v = s.StringData[ref.SecretKeyRef.Key]
				} else if s := f.secret(ref.SecretKeyRef.Name); s != nil {
					v = s.StringData[ref.SecretKeyRef.Key]
				}
			}
			env[e.Name] = mapPath(v)
		}
		return f.Exec(ctx, c, func(k string) string { return env[k] })
	}
	terminated := func(c k8s.Container, code int, msg string) k8s.ContainerStatus {
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		return k8s.ContainerStatus{Name: c.Name, State: k8s.ContainerState{Terminated: &k8s.StateTerminated{ExitCode: code, Reason: reason, Message: msg}}}
	}
	f.update(name, func(p *k8s.Pod) { p.Status.Conditions = []k8s.PodCondition{{Type: "PodScheduled", Status: "True"}} })
	for _, c := range p.Spec.InitContainers {
		code, msg := run(c)
		if ctx.Err() != nil {
			return
		}
		f.update(name, func(p *k8s.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, terminated(c, code, msg))
			if code != 0 {
				p.Status.Phase = "Failed"
			}
		})
		if code != 0 {
			return
		}
	}
	f.update(name, func(p *k8s.Pod) { p.Status.Phase = "Running" })
	for _, c := range p.Spec.Containers {
		code, msg := run(c)
		if ctx.Err() != nil {
			return
		}
		f.update(name, func(p *k8s.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, terminated(c, code, msg))
			if code != 0 {
				p.Status.Phase = "Failed"
			} else {
				p.Status.Phase = "Succeeded"
			}
		})
		if code != 0 {
			return
		}
	}
}
