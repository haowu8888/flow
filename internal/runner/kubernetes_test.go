package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"flow/internal/model"
	"flow/internal/protocol"
)

func TestPodSpec(t *testing.T) {
	r := New(Config{ServerURL: "http://flow:8080", Token: "fr_runner-token", RunnerID: "Build Farm #1",
		Kubernetes: &KubernetesConfig{HelperImage: "flow/runner:1", DefaultImage: "alpine:3.20", ServiceAccount: "ci",
			NodeSelector: map[string]string{"pool": "ci"}, ImagePullSecrets: []string{"regcred"},
			Limits: map[string]string{"memory": "1Gi"}}})
	a := &protocol.ClaimResponse{
		Job:     &model.Job{ID: "ABC123", RunID: "run1", Name: "build", Attempt: 2, JobOptions: model.JobOptions{TimeoutSec: 600}},
		Secrets: map[string]string{"DEPLOY_TOKEN": "super-secret"},
	}
	name := podName(a)
	if name != "flow-abc123-2" {
		t.Errorf("pod name = %s", name)
	}
	pod := r.podSpec(a, name)
	data, _ := json.Marshal(pod)
	for _, leak := range []string{"super-secret", "fr_runner-token"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("pod spec contains %q", leak)
		}
	}
	spec := pod.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("service account token must not be mounted")
	}
	if spec.RestartPolicy != "Never" || spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != 900 {
		t.Errorf("restart %s deadline %v", spec.RestartPolicy, spec.ActiveDeadlineSeconds)
	}
	if c := spec.Containers[0]; c.Image != "alpine:3.20" || c.Command[0] != "/flow/bin/flow-runner" || c.Resources.Limits["memory"] != "1Gi" {
		t.Errorf("job container = %+v", c)
	}
	if c := spec.InitContainers[0]; c.Image != "flow/runner:1" || c.Command[0] != "/flow-runner" || c.Command[2] != "prepare" {
		t.Errorf("init container = %+v", c)
	}
	if got := pod.Metadata.Labels["flow.runner"]; got != "Build-Farm--1" {
		t.Errorf("runner label = %q", got)
	}
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		for _, e := range c.Env {
			if e.Name == "FLOW_RUNNER_TOKEN" && (e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != name) {
				t.Errorf("%s: runner token must come from the job secret", c.Name)
			}
		}
	}
	if spec.ServiceAccountName != "ci" || spec.NodeSelector["pool"] != "ci" || spec.ImagePullSecrets[0].Name != "regcred" {
		t.Errorf("spec = %+v", spec)
	}
	// 没有配置缓存卷时不挂载缓存。
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Error("unexpected cache volume")
		}
	}
	if len(labelValue(strings.Repeat("x", 70))) != 63 {
		t.Error("label not truncated")
	}
}
