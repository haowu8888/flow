package store

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"flow/internal/model"
)

// 任务通知队列与命名构建物版本。

// NextJobNotify 锁定一个有待发送通知的任务：进入待审批（notify_waiting）或已结束（notify_done）。
func (s pgq) NextJobNotify(ctx context.Context) (*model.Job, error) {
	j, err := scanJob(s.q.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs
		WHERE (notify_waiting AND status = 'waiting') OR (notify_done AND finished_at IS NOT NULL)
		ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return j, err
}

// NextArtifactVersion 返回流水线中名为 name 的构建物的下一个版本号（v1、v2 …，只统计 vN 形式的版本）。
func (s pgq) NextArtifactVersion(ctx context.Context, pipelineID, name string) (int, error) {
	rows, err := s.q.Query(ctx, `SELECT a.version FROM artifacts a JOIN runs r ON r.id = a.run_id
		WHERE r.pipeline_id = $1 AND a.name = $2`, pipelineID, name)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return 0, err
		}
		versions = append(versions, v)
	}
	return nextVersion(versions), rows.Err()
}

func nextVersion(versions []string) int {
	n := 0
	for _, v := range versions {
		if k, err := strconv.Atoi(strings.TrimPrefix(v, "v")); err == nil && strings.HasPrefix(v, "v") && k > n {
			n = k
		}
	}
	return n + 1
}

func (m *Memory) NextJobNotify(_ context.Context) (*model.Job, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range m.jobOrder {
		j := m.jobs[id]
		if j != nil && (j.NotifyWaiting && j.Status == model.StatusWaiting || j.NotifyDone && j.FinishedAt != nil) {
			return j.Clone(), nil
		}
	}
	return nil, nil
}

func (m *Memory) NextArtifactVersion(_ context.Context, pipelineID, name string) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var versions []string
	for _, a := range m.artifacts {
		if r := m.runs[a.RunID]; r != nil && r.PipelineID == pipelineID && a.Name == name {
			versions = append(versions, a.Version)
		}
	}
	return nextVersion(versions), nil
}
