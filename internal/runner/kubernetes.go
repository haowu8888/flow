package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"flow/internal/k8s"
	"flow/internal/protocol"
	"flow/internal/secret"
)

// KubernetesConfig 启用 Kubernetes 执行器：每个 Job 在独立 Pod 中执行。
// Pod 的 init 容器（HelperImage，含 flow-runner 与 git）把 flow-runner 复制到共享卷并准备工作区（拉代码、解压上游产物、恢复缓存），
// 主容器（Job 的 image）用该二进制执行 Step 并上传产物。日志由 Pod 直接上报；领取、心跳、取消、超时与结果仍由本进程负责。
type KubernetesConfig struct {
	Client           *k8s.Client
	HelperImage      string            // 含 /flow-runner（静态链接）与 git 的镜像
	DefaultImage     string            // Job 未指定 image 时使用，须包含 sh
	ServerURL        string            // Pod 内访问 Server 的地址，默认与本进程相同
	ServiceAccount   string            // Job Pod 的 ServiceAccount；不挂载其 Token
	PullPolicy       string            // IfNotPresent / Always / Never
	CachePVC         string            // 缓存卷（多个 Pod 并发使用需 ReadWriteMany）；为空时不使用缓存
	NodeSelector     map[string]string // Job Pod 的节点选择器
	ImagePullSecrets []string
	Requests         map[string]string // 如 {"cpu": "500m", "memory": "512Mi"}
	Limits           map[string]string
	PollInterval     time.Duration // 查询 Pod 状态的间隔，默认 2s
}

const (
	podMountWorkspace  = "/workspace"
	podMountBin        = "/flow/bin"
	podMountAssignment = "/flow/assignment"
	podMountCache      = "/flow/cache"
	podHelperBinary    = "/flow-runner"

	labelManagedBy = "app.kubernetes.io/managed-by"
	labelRunner    = "flow.runner"
	labelJob       = "flow.job"
	managedBy      = "flow-runner"

	// Pod 内执行阶段使用的环境变量。
	envPodAssignment = "FLOW_POD_ASSIGNMENT"
	envPodWorkspace  = "FLOW_POD_WORKSPACE"
	envPodBin        = "FLOW_POD_BIN"
	envPodTermLog    = "FLOW_POD_TERMINATION_LOG"

	maxTerminationMessage = 1024
)

var labelUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// labelValue 把任意字符串转换为合法的标签值（字母数字开头结尾，最长 63）。
func labelValue(s string) string {
	s = labelUnsafe.ReplaceAllString(s, "-")
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "-_.")
}

func podName(a *protocol.ClaimResponse) string {
	return fmt.Sprintf("flow-%s-%d", strings.ToLower(a.Job.ID), a.Job.Attempt)
}

// cleanupOrphanPods 删除本 Runner 之前遗留的 Job Pod（如进程崩溃重启）；其 Job 的租约已过期，由 Server 回收。
func (r *Runner) cleanupOrphanPods(ctx context.Context) {
	kc := r.cfg.Kubernetes
	pods, err := kc.Client.ListPods(ctx, labelManagedBy+"="+managedBy+","+labelRunner+"="+labelValue(r.cfg.RunnerID))
	if err != nil {
		log.Printf("kubernetes: list orphan pods: %v", err)
		return
	}
	for _, p := range pods {
		log.Printf("kubernetes: deleting orphan pod %s", p.Metadata.Name)
		if err := kc.Client.DeletePod(ctx, p.Metadata.Name, 0); err != nil {
			log.Printf("kubernetes: delete orphan pod %s: %v", p.Metadata.Name, err)
		}
	}
}

// podSpec 生成 Job Pod。密钥与 Runner Token 放在同名 Secret 中，不出现在 Pod 定义里。
func (r *Runner) podSpec(a *protocol.ClaimResponse, name string) *k8s.Pod {
	kc := r.cfg.Kubernetes
	job := a.Job
	image := job.Image
	if image == "" {
		image = kc.DefaultImage
	}
	serverURL := kc.ServerURL
	if serverURL == "" {
		serverURL = r.cfg.ServerURL
	}
	env := []k8s.EnvVar{
		{Name: "FLOW_SERVER", Value: serverURL},
		{Name: "FLOW_RUNNER_TOKEN", ValueFrom: &k8s.EnvVarSource{SecretKeyRef: &k8s.SecretKeySelector{Name: name, Key: "token"}}},
		{Name: envPodAssignment, Value: podMountAssignment + "/assignment.json"},
		{Name: envPodWorkspace, Value: podMountWorkspace},
	}
	mounts := []k8s.VolumeMount{
		{Name: "workspace", MountPath: podMountWorkspace},
		{Name: "assignment", MountPath: podMountAssignment, ReadOnly: true},
	}
	mode := int32(0o444) // 主容器可能以非 root 用户运行
	volumes := []k8s.Volume{
		{Name: "workspace", EmptyDir: &struct{}{}},
		{Name: "flow-bin", EmptyDir: &struct{}{}},
		{Name: "assignment", Secret: &k8s.SecretVolumeSource{SecretName: name, DefaultMode: &mode}},
	}
	if kc.CachePVC != "" {
		volumes = append(volumes, k8s.Volume{Name: "cache", PersistentVolumeClaim: &k8s.PVCVolumeSource{ClaimName: kc.CachePVC}})
		mounts = append(mounts, k8s.VolumeMount{Name: "cache", MountPath: podMountCache})
		env = append(env, k8s.EnvVar{Name: "FLOW_CACHE_DIR", Value: podMountCache})
	}
	resources := k8s.ResourceRequirements{Requests: kc.Requests, Limits: kc.Limits}
	no := false
	grace := int64(10)
	pod := &k8s.Pod{
		APIVersion: "v1", Kind: "Pod",
		Metadata: k8s.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				labelManagedBy: managedBy, labelRunner: labelValue(r.cfg.RunnerID), labelJob: labelValue(job.ID),
			},
			Annotations: map[string]string{"flow/job-name": job.Name, "flow/run-id": job.RunID},
		},
		Spec: k8s.PodSpec{
			RestartPolicy:                 "Never",
			ServiceAccountName:            kc.ServiceAccount,
			AutomountServiceAccountToken:  &no, // Job 中的代码不应获得集群 API 权限
			TerminationGracePeriodSeconds: &grace,
			NodeSelector:                  kc.NodeSelector,
			Volumes:                       volumes,
			InitContainers: []k8s.Container{{
				Name: "prepare", Image: kc.HelperImage, ImagePullPolicy: kc.PullPolicy,
				Command:    []string{podHelperBinary, "pod", "prepare"},
				WorkingDir: podMountWorkspace,
				Env:        append(append([]k8s.EnvVar{}, env...), k8s.EnvVar{Name: envPodBin, Value: podMountBin}),
				VolumeMounts: append(append([]k8s.VolumeMount{}, mounts...),
					k8s.VolumeMount{Name: "flow-bin", MountPath: podMountBin}),
				Resources:                resources,
				TerminationMessagePolicy: "FallbackToLogsOnError",
			}},
			Containers: []k8s.Container{{
				Name: "job", Image: image, ImagePullPolicy: kc.PullPolicy,
				Command:    []string{podMountBin + "/flow-runner", "pod", "run"},
				WorkingDir: podMountWorkspace,
				Env:        env,
				VolumeMounts: append(append([]k8s.VolumeMount{}, mounts...),
					k8s.VolumeMount{Name: "flow-bin", MountPath: podMountBin, ReadOnly: true}),
				Resources:                resources,
				TerminationMessagePolicy: "FallbackToLogsOnError",
			}},
		},
	}
	for _, s := range kc.ImagePullSecrets {
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, k8s.LocalObjectRef{Name: s})
	}
	if job.TimeoutSec > 0 {
		// 兜底：本进程负责精确超时；即使本进程退出，Pod 也不会无限运行。
		d := int64(job.TimeoutSec + 300)
		pod.Spec.ActiveDeadlineSeconds = &d
	}
	return pod
}

// 这些等待原因表示 Pod 无法自行恢复，直接判定 Job 失败。
var fatalWaiting = map[string]bool{
	"ImagePullBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true,
}

// runPod 在 Kubernetes Pod 中执行 Job，返回失败 Step 的退出码。ctx 结束（取消、超时）时删除 Pod。
func (r *Runner) runPod(ctx context.Context, a *protocol.ClaimResponse, out io.Writer) (int, error) {
	kc := r.cfg.Kubernetes
	name := podName(a)
	spec := r.podSpec(a, name)
	// 上一次进程崩溃可能遗留同名 Pod（同一 attempt 被回收后不会再次下发，这里只是防御）。
	_ = kc.Client.DeletePod(ctx, name, 0)
	pod, err := kc.Client.CreatePod(ctx, spec)
	if err != nil {
		return -1, fmt.Errorf("create pod: %w", err)
	}
	defer func() {
		// 已结束或被取消都删除 Pod；Secret 通过 ownerReference 随 Pod 回收。
		dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := kc.Client.DeletePod(dctx, name, 5); err != nil {
			log.Printf("kubernetes: delete pod %s: %v", name, err)
		}
	}()

	assignment, err := json.Marshal(a)
	if err != nil {
		return -1, err
	}
	sec := &k8s.Secret{
		APIVersion: "v1", Kind: "Secret", Type: "Opaque",
		Metadata: k8s.ObjectMeta{
			Name: name, Labels: spec.Metadata.Labels,
			OwnerReferences: []k8s.OwnerReference{{APIVersion: "v1", Kind: "Pod", Name: name, UID: pod.Metadata.UID}},
		},
		StringData: map[string]string{"assignment.json": string(assignment), "token": r.cfg.Token},
	}
	if err := kc.Client.CreateSecret(ctx, sec); err != nil {
		return -1, fmt.Errorf("create secret: %w", err)
	}
	fmt.Fprintf(out, "==> kubernetes pod %s/%s (image %s)\n", kc.Client.Namespace(), name, spec.Spec.Containers[0].Image)

	poll := kc.PollInterval
	if poll <= 0 {
		poll = 2 * time.Second
	}
	var lastNote string
	note := func(s string) {
		if s != lastNote {
			lastNote = s
			fmt.Fprintf(out, "==> %s\n", s)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(poll):
		}
		p, err := kc.Client.GetPod(ctx, name)
		if errors.Is(err, k8s.ErrNotFound) {
			return -1, errors.New("pod was deleted externally")
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("kubernetes: get pod %s: %v", name, err)
			}
			continue
		}
		st := p.Status
		statuses := append(append([]k8s.ContainerStatus{}, st.InitContainerStatuses...), st.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && fatalWaiting[w.Reason] {
				return -1, fmt.Errorf("container %s: %s: %s", cs.Name, w.Reason, w.Message)
			}
		}
		switch st.Phase {
		case "Succeeded":
			return 0, nil
		case "Failed":
			for _, cs := range statuses {
				if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
					msg := strings.TrimSpace(t.Message)
					if msg == "" {
						msg = fmt.Sprintf("container %s exited with code %d (%s)", cs.Name, t.ExitCode, t.Reason)
					}
					return t.ExitCode, errors.New(msg)
				}
			}
			reason := strings.TrimSpace(st.Reason + " " + st.Message)
			if reason == "" {
				reason = "unknown reason"
			}
			return -1, fmt.Errorf("pod failed: %s", reason)
		case "Pending":
			for _, c := range st.Conditions {
				if c.Type == "PodScheduled" && c.Status == "False" && c.Message != "" {
					note("waiting for scheduling: " + c.Message)
				}
			}
			for _, cs := range statuses {
				if w := cs.State.Waiting; w != nil && w.Reason == "ErrImagePull" {
					note(fmt.Sprintf("pulling image for %s failed, retrying: %s", cs.Name, w.Message))
				}
			}
		}
	}
}

// RunPod 是 Job Pod 内的入口（flow-runner pod prepare|run）：prepare 在 init 容器中准备工作区，run 在主容器中执行 Step。
// 返回进程退出码与写入终止消息的失败原因。getenv 通常为 os.Getenv。
func RunPod(ctx context.Context, phase string, getenv func(string) string) (code int, message string) {
	fail := func(code int, err error) (int, string) {
		msg := err.Error()
		if len(msg) > maxTerminationMessage {
			msg = msg[:maxTerminationMessage]
		}
		if code <= 0 {
			code = 1
		}
		return code, msg
	}
	data, err := os.ReadFile(getenv(envPodAssignment))
	if err != nil {
		return fail(1, fmt.Errorf("read assignment: %w", err))
	}
	var a protocol.ClaimResponse
	if err := json.Unmarshal(data, &a); err != nil || a.Job == nil {
		return fail(1, fmt.Errorf("invalid assignment: %v", err))
	}
	dir := getenv(envPodWorkspace)
	r := New(Config{
		ServerURL: getenv("FLOW_SERVER"), Token: getenv("FLOW_RUNNER_TOKEN"), WorkDir: dir,
		CacheDir: getenv("FLOW_CACHE_DIR"), NoCache: getenv("FLOW_CACHE_DIR") == "",
	})
	job := a.Job

	// Job 已被回收（如控制进程失联导致租约过期）时日志上报返回 409，随即中止。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	logs := newLogUploader(func(c context.Context, data []byte) error {
		err := r.client.AppendLog(c, job.ID, job.RunnerID, data)
		if errors.Is(err, ErrLeaseLost) {
			cancel()
		}
		return err
	}, r.cfg.LogFlushInterval)
	a.ApplyConnections()
	out := secret.NewMasker(logs, a.MaskValues())
	defer func() {
		out.Flush()
		logs.Close()
	}()

	switch phase {
	case "prepare":
		if bin := getenv(envPodBin); bin != "" {
			if err := copySelf(filepath.Join(bin, "flow-runner")); err != nil {
				return fail(1, fmt.Errorf("install flow-runner: %w", err))
			}
		}
		if err := r.prepareWorkspace(ctx, &a, dir, out); err != nil {
			return fail(1, err)
		}
	case "run":
		local := *job
		local.Image = "" // 已在 Job 的镜像中，直接执行
		la := a
		la.Job = &local
		code, err := r.runSteps(ctx, &la, dir, out)
		r.uploadReports(job, dir, out)
		if err != nil {
			return fail(code, err) // 失败原因经终止消息交给控制进程，由其写入日志
		}
	default:
		return fail(2, fmt.Errorf("unknown pod phase %q (want prepare or run)", phase))
	}
	if ctx.Err() != nil {
		return fail(1, errors.New("job is no longer leased by this runner"))
	}
	return 0, ""
}

// WriteTerminationMessage 把失败原因写入 Kubernetes 的终止消息文件，控制进程据此生成 Job 的错误信息。
func WriteTerminationMessage(getenv func(string) string, msg string) {
	path := getenv(envPodTermLog)
	if path == "" {
		path = "/dev/termination-log"
	}
	_ = os.WriteFile(path, []byte(msg), 0o644)
}

func copySelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp-" + strconv.Itoa(os.Getpid())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, in); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
