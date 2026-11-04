// Command server 启动 Flow 控制面：HTTP API + Web UI + 调度引擎 + 后台循环（租约回收、cron、通知）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // cron 时区：精简镜像中也能加载 IANA 时区

	"github.com/jackc/pgx/v5/pgxpool"

	"flow/internal/api"
	"flow/internal/auth"
	"flow/internal/blob"
	"flow/internal/engine"
	"flow/internal/pipeline"
	"flow/internal/secret"
	"flow/internal/store"
	"flow/internal/webui"
)

func main() {
	addr := flag.String("addr", envOr("FLOW_ADDR", ":8080"), "listen address")
	token := flag.String("runner-token", os.Getenv("FLOW_RUNNER_TOKEN"), "shared token for runners of all tenants (optional)")
	dbURL := flag.String("database-url", os.Getenv("FLOW_DATABASE_URL"), "PostgreSQL URL; empty uses in-memory store")
	dataDir := flag.String("data-dir", envOr("FLOW_DATA_DIR", "data"), "directory for the secret key file and local artifacts")
	publicURL := flag.String("public-url", os.Getenv("FLOW_PUBLIC_URL"), "external URL of the web UI, used in notification links")
	runnerDist := flag.String("runner-dist", os.Getenv("FLOW_RUNNER_DIST"), "directory with flow-runner-<os>-<arch> binaries served to the install script")
	retention := flag.String("artifact-retention", envOr("FLOW_ARTIFACT_RETENTION", "30d"), "default artifact retention (e.g. 30d, 72h; 0 keeps forever)")
	flag.Parse()

	artifactTTL, err := pipeline.ParseRetention(*retention)
	if err != nil {
		log.Fatalf("artifact-retention: %v", err)
	}
	if artifactTTL == pipeline.ForeverTTL {
		artifactTTL = 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, closeStore, err := openStore(ctx, *dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer closeStore()
	cipher, err := openCipher(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	blobs, err := openBlobs(*dataDir)
	if err != nil {
		log.Fatal(err)
	}
	eng := engine.New(st, 30*time.Second, engine.WithCipher(cipher), engine.WithBlobs(blobs), engine.WithPublicURL(*publicURL),
		engine.WithArtifactRetention(artifactTTL))

	authSvc := auth.NewService(st)
	created, generated, err := authSvc.Bootstrap(ctx, os.Getenv("FLOW_ADMIN_PASSWORD"))
	switch {
	case err != nil:
		log.Fatal(err)
	case generated != "":
		log.Printf("created admin user: admin / %s (change it after logging in)", generated)
	case created:
		log.Printf("created admin user: admin (password from FLOW_ADMIN_PASSWORD)")
	}
	if *token == "" {
		log.Printf("FLOW_RUNNER_TOKEN not set: only runners registered in the UI can connect")
	}

	// 后台循环：回收失联 Runner 的租约、触发到期的 cron、发送运行结束通知（多实例同时运行是安全的）。
	go every(ctx, 5*time.Second, func() {
		if n, err := eng.ReapExpired(ctx); err != nil {
			log.Printf("reap: %v", err)
		} else if n > 0 {
			log.Printf("reaped expired jobs in %d run(s)", n)
		}
		if n, err := eng.FireDueCron(ctx); err != nil {
			log.Printf("cron: %v", err)
		} else if n > 0 {
			log.Printf("fired %d cron trigger(s)", n)
		}
		if _, err := eng.DeliverNotifications(ctx); err != nil {
			log.Printf("notify: %v", err)
		}
		if _, err := eng.DeliverJobNotifications(ctx); err != nil {
			log.Printf("job notify: %v", err)
		}
		if _, err := eng.DispatchGitHub(ctx); err != nil {
			log.Printf("github nodes: %v", err)
		}
		if n, err := eng.ExpireHostTasks(ctx); err != nil {
			log.Printf("host tasks: %v", err)
		} else if n > 0 {
			log.Printf("expired %d host task(s) of lost agents", n)
		}
	})
	go every(ctx, time.Hour, func() {
		if arts, runs, err := eng.ApplyRetention(ctx); err != nil {
			log.Printf("retention: %v", err)
		} else if arts > 0 || runs > 0 {
			log.Printf("retention: removed %d expired artifact(s) and %d old run(s)", arts, runs)
		}
		if err := eng.CleanupDeliveries(ctx); err != nil {
			log.Printf("webhook delivery cleanup: %v", err)
		}
		if err := authSvc.Cleanup(ctx); err != nil {
			log.Printf("token cleanup: %v", err)
		}
	})

	apiHandler := api.NewHandler(eng, authSvc, api.Config{
		RunnerToken:  *token,
		RunnerDist:   *runnerDist,
		SecureCookie: strings.HasPrefix(*publicURL, "https://"),
		Done:         ctx.Done(),
	})
	root := http.NewServeMux()
	root.Handle("/api/", apiHandler)
	root.Handle("/healthz", apiHandler)
	root.Handle("/", webui.Handler())
	srv := &http.Server{Addr: *addr, Handler: root, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Printf("flow server listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func every(ctx context.Context, d time.Duration, fn func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// openCipher 使用 FLOW_SECRET_KEY，未设置时读取或生成 data-dir/secret.key。
// 多实例部署须共享同一主密钥；主密钥丢失后已保存的密钥无法解密。
func openCipher(dataDir string) (*secret.Cipher, error) {
	if v := os.Getenv("FLOW_SECRET_KEY"); v != "" {
		key, err := secret.ParseKey(v)
		if err != nil {
			return nil, fmt.Errorf("FLOW_SECRET_KEY: %w", err)
		}
		return secret.NewCipher(key)
	}
	path := filepath.Join(dataDir, "secret.key")
	key, created, err := secret.LoadOrCreateKeyFile(path)
	if err != nil {
		return nil, fmt.Errorf("secret key file %s: %w", path, err)
	}
	if created {
		log.Printf("generated secret encryption key at %s (back it up; set FLOW_SECRET_KEY when running multiple instances)", path)
	}
	return secret.NewCipher(key)
}

// openBlobs 配置了 FLOW_S3_BUCKET 时使用 S3 兼容存储，否则存于 data-dir/artifacts。
func openBlobs(dataDir string) (blob.Store, error) {
	if bucket := os.Getenv("FLOW_S3_BUCKET"); bucket != "" {
		s3, err := blob.NewS3(blob.S3Config{
			Endpoint: os.Getenv("FLOW_S3_ENDPOINT"), Region: os.Getenv("FLOW_S3_REGION"), Bucket: bucket,
			AccessKey: os.Getenv("FLOW_S3_ACCESS_KEY"), SecretKey: os.Getenv("FLOW_S3_SECRET_KEY"),
			PathStyle: os.Getenv("FLOW_S3_PATH_STYLE") != "false", Prefix: os.Getenv("FLOW_S3_PREFIX"),
		})
		if err != nil {
			return nil, err
		}
		log.Printf("storing artifacts in S3 bucket %s", bucket)
		return s3, nil
	}
	return blob.NewLocal(filepath.Join(dataDir, "artifacts"))
}

// openStore 连接 PostgreSQL 并执行迁移；url 为空时退回内存存储（数据不持久化）。
func openStore(ctx context.Context, url string) (store.Store, func(), error) {
	if url == "" {
		log.Printf("FLOW_DATABASE_URL not set, using in-memory store (data is lost on restart)")
		return store.NewMemory(), func() {}, nil
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	if err := store.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	log.Printf("using PostgreSQL store")
	return store.NewPostgres(pool), pool.Close, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
