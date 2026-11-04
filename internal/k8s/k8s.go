// Package k8s 是 Kubernetes 执行器所需的最小 API 客户端：只覆盖 Pod 与 Secret 的增删查，
// 直接调用 REST API，避免引入 client-go。
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var ErrNotFound = errors.New("not found")

const (
	saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

type Client struct {
	base      string
	token     string
	namespace string
	http      *http.Client
}

// Config 用于集群外连接（测试或本地开发）；在集群内运行时使用 InCluster。
type Config struct {
	APIURL    string // 如 https://127.0.0.1:6443
	Token     string
	Namespace string
	CAFile    string // 为空时使用系统根证书
	Insecure  bool   // 跳过证书校验，仅用于测试
}

func New(cfg Config) (*Client, error) {
	if cfg.APIURL == "" || cfg.Namespace == "" {
		return nil, errors.New("k8s: api url and namespace are required")
	}
	tlsCfg := &tls.Config{InsecureSkipVerify: cfg.Insecure} //nolint:gosec // 仅在显式配置时
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("k8s: read ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("k8s: invalid ca certificate")
		}
		tlsCfg.RootCAs = pool
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &Client{
		base: strings.TrimRight(cfg.APIURL, "/"), token: cfg.Token, namespace: cfg.Namespace,
		http: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

// InCluster 使用 Pod 的 ServiceAccount 连接所在集群；namespace 为空时使用 Pod 所在命名空间。
func InCluster(namespace string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("k8s: not running in a cluster (KUBERNETES_SERVICE_HOST unset); use explicit api settings")
	}
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("k8s: read service account token: %w", err)
	}
	if namespace == "" {
		ns, err := os.ReadFile(saDir + "/namespace")
		if err != nil {
			return nil, fmt.Errorf("k8s: read namespace: %w", err)
		}
		namespace = strings.TrimSpace(string(ns))
	}
	return New(Config{APIURL: "https://" + host + ":" + port, Token: strings.TrimSpace(string(token)),
		Namespace: namespace, CAFile: saDir + "/ca.crt"})
}

func (c *Client) Namespace() string { return c.namespace }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s", ErrNotFound, method, path)
	case resp.StatusCode >= 300:
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &st) == nil && st.Message != "" {
			return fmt.Errorf("k8s: %s %s: %s (%d)", method, path, st.Message, resp.StatusCode)
		}
		return fmt.Errorf("k8s: %s %s: %s", method, path, resp.Status)
	case out != nil:
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) nsPath(kind string) string {
	return "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/" + kind
}

func (c *Client) CreatePod(ctx context.Context, p *Pod) (*Pod, error) {
	var out Pod
	return &out, c.do(ctx, http.MethodPost, c.nsPath("pods"), p, &out)
}

func (c *Client) GetPod(ctx context.Context, name string) (*Pod, error) {
	var out Pod
	return &out, c.do(ctx, http.MethodGet, c.nsPath("pods")+"/"+url.PathEscape(name), nil, &out)
}

// DeletePod 删除 Pod；不存在时不报错。
func (c *Client) DeletePod(ctx context.Context, name string, gracePeriod int) error {
	err := c.do(ctx, http.MethodDelete, c.nsPath("pods")+"/"+url.PathEscape(name),
		map[string]any{"gracePeriodSeconds": gracePeriod, "propagationPolicy": "Background"}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) ListPods(ctx context.Context, labelSelector string) ([]Pod, error) {
	var out struct {
		Items []Pod `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, c.nsPath("pods")+"?labelSelector="+url.QueryEscape(labelSelector), nil, &out)
	return out.Items, err
}

func (c *Client) CreateSecret(ctx context.Context, s *Secret) error {
	return c.do(ctx, http.MethodPost, c.nsPath("secrets"), s, nil)
}

// 以下为所需字段的最小子集，字段名与 Kubernetes API 一致。

type ObjectMeta struct {
	Name            string            `json:"name,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
	OwnerReferences []OwnerReference  `json:"ownerReferences,omitempty"`
}

type OwnerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type Secret struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	StringData map[string]string `json:"stringData,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"` // API 返回时为 base64，encoding/json 自动编解码
}

type Pod struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
	Status     PodStatus  `json:"status,omitempty"`
}

type PodSpec struct {
	RestartPolicy                 string            `json:"restartPolicy,omitempty"`
	ActiveDeadlineSeconds         *int64            `json:"activeDeadlineSeconds,omitempty"`
	TerminationGracePeriodSeconds *int64            `json:"terminationGracePeriodSeconds,omitempty"`
	ServiceAccountName            string            `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken  *bool             `json:"automountServiceAccountToken,omitempty"`
	NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
	ImagePullSecrets              []LocalObjectRef  `json:"imagePullSecrets,omitempty"`
	Volumes                       []Volume          `json:"volumes,omitempty"`
	InitContainers                []Container       `json:"initContainers,omitempty"`
	Containers                    []Container       `json:"containers"`
}

type LocalObjectRef struct {
	Name string `json:"name"`
}

type Volume struct {
	Name                  string              `json:"name"`
	EmptyDir              *struct{}           `json:"emptyDir,omitempty"`
	Secret                *SecretVolumeSource `json:"secret,omitempty"`
	PersistentVolumeClaim *PVCVolumeSource    `json:"persistentVolumeClaim,omitempty"`
}

type SecretVolumeSource struct {
	SecretName  string `json:"secretName"`
	DefaultMode *int32 `json:"defaultMode,omitempty"`
}

type PVCVolumeSource struct {
	ClaimName string `json:"claimName"`
}

type Container struct {
	Name                     string               `json:"name"`
	Image                    string               `json:"image"`
	ImagePullPolicy          string               `json:"imagePullPolicy,omitempty"`
	Command                  []string             `json:"command,omitempty"`
	WorkingDir               string               `json:"workingDir,omitempty"`
	Env                      []EnvVar             `json:"env,omitempty"`
	VolumeMounts             []VolumeMount        `json:"volumeMounts,omitempty"`
	Resources                ResourceRequirements `json:"resources,omitempty"`
	TerminationMessagePolicy string               `json:"terminationMessagePolicy,omitempty"`
}

type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty"`
}

type SecretKeySelector struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"` // Pending / Running / Succeeded / Failed / Unknown
	Message               string            `json:"message,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
}

type PodCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type ContainerStatus struct {
	Name  string         `json:"name"`
	State ContainerState `json:"state"`
}

type ContainerState struct {
	Waiting    *StateWaiting    `json:"waiting,omitempty"`
	Running    *struct{}        `json:"running,omitempty"`
	Terminated *StateTerminated `json:"terminated,omitempty"`
}

type StateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type StateTerminated struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
}
