package engine

import (
	"context"
	"strconv"
	"time"

	"flow/internal/model"
	"flow/internal/store"
)

// DriveUpload 是一次网盘上传（cloudreve-upload 步骤）的结果。
type DriveUpload struct {
	JobName  string `json:"job_name"`
	Step     string `json:"step"`
	Dir      string `json:"dir"`
	Files    int    `json:"files"`
	Size     int64  `json:"size,omitempty"`
	URL      string `json:"url,omitempty"`       // 网盘中打开该目录的地址
	ShareURL string `json:"share_url,omitempty"` // 分享链接（勾选生成分享链接时）
}

// ObjectUpload 是一次对象存储上传（oss-upload 步骤）的结果。
type ObjectUpload struct {
	JobName string `json:"job_name"`
	Step    string `json:"step"`
	Bucket  string `json:"bucket,omitempty"`
	Path    string `json:"path"`
	URL     string `json:"url,omitempty"` // 连接配置了公开访问域名时
}

// Delivery 是一次运行交付的全部内容：构建物、网盘 / 对象存储上传与主机部署，即流水线的「部署历史」一行。
type Delivery struct {
	Seq         int                 `json:"seq"` // 部署记录编号，从 1 开始
	Run         *model.Run          `json:"run"`
	StartedAt   *time.Time          `json:"started_at,omitempty"` // 首个交付任务开始的时间
	Artifacts   []*model.Artifact   `json:"artifacts"`
	Drives      []DriveUpload       `json:"drives"`
	Objects     []ObjectUpload      `json:"objects"`
	Deployments []*model.Deployment `json:"deployments"`
}

// maxDeliveryScan 是部署历史回看的最近运行数。
const maxDeliveryScan = 300

// Deliveries 返回流水线最近有交付内容的运行，新的在前；total 为回看范围内的记录总数。
func (e *Engine) Deliveries(ctx context.Context, p *model.Pipeline, offset, limit int) ([]*Delivery, int, error) {
	runs, err := e.store.ListRuns(ctx, p.ID, maxDeliveryScan)
	if err != nil {
		return nil, 0, err
	}
	deps, err := e.store.ListDeployments(ctx, p.TenantID, store.DeploymentFilter{PipelineID: p.ID, Limit: 1000})
	if err != nil {
		return nil, 0, err
	}
	byRun := map[string][]*model.Deployment{}
	for _, d := range deps {
		byRun[d.RunID] = append(byRun[d.RunID], d)
	}
	var out []*Delivery
	for _, r := range runs {
		d, err := e.delivery(ctx, r, byRun[r.ID])
		if err != nil {
			return nil, 0, err
		}
		if d != nil {
			out = append(out, d)
		}
	}
	for i, d := range out {
		d.Seq = len(out) - i
	}
	total := len(out)
	if offset >= total {
		return []*Delivery{}, total, nil
	}
	return out[offset:min(total, offset+limit)], total, nil
}

func (e *Engine) delivery(ctx context.Context, r *model.Run, deps []*model.Deployment) (*Delivery, error) {
	arts, err := e.store.ListArtifacts(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	jobs, err := e.store.ListJobs(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	d := &Delivery{Run: r, Artifacts: arts, Drives: []DriveUpload{}, Objects: []ObjectUpload{}, Deployments: deps}
	if d.Artifacts == nil {
		d.Artifacts = []*model.Artifact{}
	}
	if d.Deployments == nil {
		d.Deployments = []*model.Deployment{}
	}
	jobByID := map[string]*model.Job{}
	start := func(j *model.Job) {
		if j != nil && j.StartedAt != nil && (d.StartedAt == nil || j.StartedAt.Before(*d.StartedAt)) {
			d.StartedAt = j.StartedAt
		}
	}
	for _, j := range jobs {
		jobByID[j.ID] = j
		for i, st := range j.StepStates {
			if i >= len(j.Steps) || st.Status != model.StatusSuccess || len(st.Outputs) == 0 {
				continue
			}
			step, o := j.Steps[i], st.Outputs
			switch step.Kind {
			case "cloudreve-upload":
				files, _ := strconv.Atoi(o["CLOUDREVE_FILES"])
				size, _ := strconv.ParseInt(o["CLOUDREVE_SIZE"], 10, 64)
				d.Drives = append(d.Drives, DriveUpload{JobName: j.Name, Step: step.Name, Dir: o["CLOUDREVE_DIR"], Files: files, Size: size,
					URL: o["CLOUDREVE_URL"], ShareURL: o["CLOUDREVE_SHARE_URL"]})
				start(j)
			case "oss-upload":
				path := o["OSS_OBJECT"]
				if path == "" {
					path = step.With["object_path"]
				}
				d.Objects = append(d.Objects, ObjectUpload{JobName: j.Name, Step: step.Name, Bucket: o["OSS_BUCKET"], Path: path, URL: o["OSS_URL"]})
				start(j)
			}
		}
	}
	for _, a := range arts {
		start(jobByID[a.JobID])
	}
	for _, dep := range deps {
		if d.StartedAt == nil || dep.StartedAt.Before(*d.StartedAt) {
			t := dep.StartedAt
			d.StartedAt = &t
		}
	}
	if len(arts) == 0 && len(d.Drives) == 0 && len(d.Objects) == 0 && len(deps) == 0 {
		return nil, nil
	}
	if d.StartedAt == nil {
		d.StartedAt = r.StartedAt
	}
	return d, nil
}
