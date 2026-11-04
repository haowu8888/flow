// Command runner 启动执行节点，主动向 Server 拉取任务（可部署在内网，无需入站端口）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"flow/internal/k8s"
	"flow/internal/runner"
	"flow/internal/version"
)

func main() {
	// Kubernetes 执行器的 Job Pod 内：flow-runner pod prepare|run
	if len(os.Args) == 3 && os.Args[1] == "pod" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code, msg := runner.RunPod(ctx, os.Args[2], os.Getenv)
		stop()
		if msg != "" {
			runner.WriteTerminationMessage(os.Getenv, msg)
			log.Print(msg)
		}
		os.Exit(code)
	}

	host, _ := os.Hostname()
	concurrency, _ := strconv.Atoi(envOr("FLOW_CONCURRENCY", "1"))

	var cfg runner.Config
	flag.StringVar(&cfg.ServerURL, "server", envOr("FLOW_SERVER", "http://localhost:8080"), "server base URL")
	flag.StringVar(&cfg.Token, "token", os.Getenv("FLOW_RUNNER_TOKEN"), "runner token")
	flag.StringVar(&cfg.RunnerID, "id", envOr("FLOW_RUNNER_ID", host), "runner id (only used with the shared token)")
	labels := flag.String("labels", os.Getenv("FLOW_RUNNER_LABELS"), "comma-separated labels matched against runs-on")
	flag.StringVar(&cfg.CacheDir, "cache-dir", os.Getenv("FLOW_CACHE_DIR"), "local cache directory (default <workdir>/cache)")
	flag.StringVar(&cfg.WorkDir, "workdir", envOr("FLOW_WORKDIR", filepath.Join(os.TempDir(), "flow-runner")), "job workspace root")
	flag.IntVar(&cfg.Concurrency, "concurrency", concurrency, "parallel jobs")
	flag.BoolVar(&cfg.Once, "once", os.Getenv("FLOW_RUNNER_ONCE") == "1", "claim and run a single job, then exit (GitHub Actions nodes)")
	flag.DurationVar(&cfg.OnceWait, "wait", 5*time.Minute, "with -once: how long to wait for a job before giving up")
	flag.BoolVar(&cfg.Agent, "agent", os.Getenv("FLOW_MODE") == "agent", "run as a host agent: execute deployments targeting this host")
	showVersion := flag.Bool("version", false, "print the version and exit")
	executor := flag.String("executor", envOr("FLOW_EXECUTOR", "shell"), "shell (host / docker) or kubernetes (one pod per job)")
	var kc runner.KubernetesConfig
	k8sNamespace := flag.String("k8s-namespace", os.Getenv("FLOW_K8S_NAMESPACE"), "namespace for job pods (default: the runner's own namespace)")
	k8sAPI := flag.String("k8s-api", os.Getenv("FLOW_K8S_API"), "Kubernetes API URL when running outside the cluster")
	k8sToken := flag.String("k8s-token", os.Getenv("FLOW_K8S_TOKEN"), "bearer token for -k8s-api")
	k8sCA := flag.String("k8s-ca", os.Getenv("FLOW_K8S_CA"), "CA certificate file for -k8s-api")
	flag.StringVar(&kc.HelperImage, "k8s-helper-image", os.Getenv("FLOW_K8S_HELPER_IMAGE"), "image with /flow-runner and git, used by the prepare init container")
	flag.StringVar(&kc.DefaultImage, "k8s-default-image", envOr("FLOW_K8S_DEFAULT_IMAGE", "alpine:3.20"), "image for jobs without image")
	flag.StringVar(&kc.ServerURL, "k8s-server-url", os.Getenv("FLOW_K8S_SERVER_URL"), "server URL as reachable from job pods (default: -server)")
	flag.StringVar(&kc.ServiceAccount, "k8s-service-account", os.Getenv("FLOW_K8S_SERVICE_ACCOUNT"), "service account of job pods (its token is not mounted)")
	flag.StringVar(&kc.PullPolicy, "k8s-pull-policy", os.Getenv("FLOW_K8S_PULL_POLICY"), "image pull policy for job pods")
	flag.StringVar(&kc.CachePVC, "k8s-cache-pvc", os.Getenv("FLOW_K8S_CACHE_PVC"), "PersistentVolumeClaim for the job cache (ReadWriteMany); empty disables cache")
	nodeSelector := flag.String("k8s-node-selector", os.Getenv("FLOW_K8S_NODE_SELECTOR"), "node selector for job pods, e.g. pool=ci,arch=amd64")
	pullSecrets := flag.String("k8s-image-pull-secrets", os.Getenv("FLOW_K8S_IMAGE_PULL_SECRETS"), "comma-separated image pull secrets")
	requests := flag.String("k8s-requests", os.Getenv("FLOW_K8S_REQUESTS"), "resource requests, e.g. cpu=500m,memory=512Mi")
	limits := flag.String("k8s-limits", os.Getenv("FLOW_K8S_LIMITS"), "resource limits, e.g. cpu=2,memory=2Gi")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	cfg.Labels = list(*labels)
	if cfg.Token == "" {
		log.Fatal("runner token is required (-token or FLOW_RUNNER_TOKEN)")
	}
	switch *executor {
	case "shell":
	case "kubernetes":
		if kc.HelperImage == "" {
			log.Fatal("kubernetes executor requires -k8s-helper-image (an image with /flow-runner and git)")
		}
		var err error
		if *k8sAPI != "" {
			kc.Client, err = k8s.New(k8s.Config{APIURL: *k8sAPI, Token: *k8sToken, Namespace: envOr("FLOW_K8S_NAMESPACE", *k8sNamespace), CAFile: *k8sCA})
		} else {
			kc.Client, err = k8s.InCluster(*k8sNamespace)
		}
		if err != nil {
			log.Fatal(err)
		}
		kc.NodeSelector, kc.Requests, kc.Limits = keyValues(*nodeSelector), keyValues(*requests), keyValues(*limits)
		kc.ImagePullSecrets = list(*pullSecrets)
		cfg.Kubernetes = &kc
		log.Printf("kubernetes executor: namespace %s, helper image %s", kc.Client.Namespace(), kc.HelperImage)
	default:
		log.Fatalf("unknown executor %q (want shell or kubernetes)", *executor)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !cfg.Agent {
		log.Printf("runner polling %s (concurrency %d, labels %v)", cfg.ServerURL, cfg.Concurrency, cfg.Labels)
	}
	if err := runner.New(cfg).Run(ctx); errors.Is(err, runner.ErrNoJob) {
		log.Print("no job to claim, exiting")
	} else if err != nil {
		log.Fatal(err)
	}
}

func list(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// keyValues 解析 a=b,c=d。
func keyValues(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range list(s) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			log.Fatalf("invalid key=value %q", kv)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
