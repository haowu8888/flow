package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// 流水线成员与角色：私有流水线只有拥有者、成员、租户管理员可见；成员按角色授权；公开后其他人只读。
func TestPipelineAccess(t *testing.T) { storetest.ForEach(t, testPipelineAccess) }

func testPipelineAccess(t *testing.T, s store.Store) {
	ts := newStack(t, s)
	for _, u := range []string{"dev1", "dev2", "dev3"} {
		postRaw(t, ts, "/api/members", `{"username":"`+u+`","password":"password-`+u+`","role":"developer"}`, http.StatusCreated)
	}
	dev1 := ts.as(ts.login(t, "dev1", "password-dev1"))
	dev2 := ts.as(ts.login(t, "dev2", "password-dev2"))
	dev3 := ts.as(ts.login(t, "dev3", "password-dev3"))
	ids := map[string]string{}
	var members []struct {
		UserID   string `json:"user_id"`
		Username string `json:"username"`
	}
	json.Unmarshal([]byte(getText(t, ts, "/api/members")), &members)
	for _, m := range members {
		ids[m.Username] = m.UserID
	}

	// dev1 创建：自己是拥有者，其他开发者看不到。
	yaml := "name: private\nstages: [{name: s, jobs: [{name: j, steps: [{run: 'true'}]}]}]\n"
	code, body := dev1.do(t, http.MethodPost, "/api/pipelines", yaml)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var p model.Pipeline
	json.Unmarshal([]byte(body), &p)
	base := "/api/pipelines/" + p.ID
	if code, _ := dev2.do(t, http.MethodGet, base, ""); code != http.StatusNotFound {
		t.Errorf("non-member read = %d", code)
	}
	if list := getTextAs(t, dev2, "/api/pipelines/summary"); strings.Contains(list, p.ID) {
		t.Errorf("summary leaks private pipeline: %s", list)
	}
	if v := getTextAs(t, dev1, base); !strings.Contains(v, `"permissions":["view","run","edit","delete","deleteRun","manageMembers"]`) {
		t.Errorf("owner view = %s", v)
	}
	// 租户管理员可见
	if code, _ := ts.do(t, http.MethodGet, base, ""); code != http.StatusOK {
		t.Errorf("tenant admin read = %d", code)
	}

	// 成员：可查看、运行，不能修改
	if code, b := dev1.do(t, http.MethodPut, base+"/members/"+ids["dev2"], `{"role_id":"member"}`); code != http.StatusNoContent {
		t.Fatalf("add member = %d %s", code, b)
	}
	if code, _ := dev1.do(t, http.MethodPut, base+"/members/"+ids["dev3"], `{"role_id":"owner"}`); code != http.StatusBadRequest {
		t.Errorf("set owner role = %d", code)
	}
	if code, _ := dev2.do(t, http.MethodPost, base+"/runs", ""); code != http.StatusCreated {
		t.Errorf("member run = %d", code)
	}
	if code, _ := dev2.do(t, http.MethodPut, base, yaml); code != http.StatusForbidden {
		t.Errorf("member edit = %d", code)
	}
	if code, _ := dev2.do(t, http.MethodPut, base+"/members/"+ids["dev3"], `{"role_id":"member"}`); code != http.StatusForbidden {
		t.Errorf("member manage members = %d", code)
	}

	// 自定义角色：可编辑但不能运行
	code, body = ts.do(t, http.MethodPost, "/api/pipeline-roles", `{"name":"配置员","permissions":["edit"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create role = %d %s", code, body)
	}
	var role model.PipelineRole
	json.Unmarshal([]byte(body), &role)
	if strings.Join(role.Permissions, ",") != "view,edit" {
		t.Errorf("role permissions = %v", role.Permissions)
	}
	if code, _ := dev1.do(t, http.MethodPost, "/api/pipeline-roles", `{"name":"x"}`); code != http.StatusForbidden {
		t.Errorf("developer create role = %d", code)
	}
	dev1.do(t, http.MethodPut, base+"/members/"+ids["dev2"], `{"role_id":"`+role.ID+`"}`)
	if code, _ := dev2.do(t, http.MethodPost, base+"/runs", ""); code != http.StatusForbidden {
		t.Errorf("editor run = %d", code)
	}
	if code, _ := dev2.do(t, http.MethodPut, base, strings.Replace(yaml, "'true'", "'echo'", 1)); code != http.StatusOK {
		t.Errorf("editor edit = %d", code)
	}
	if code, _ := ts.do(t, http.MethodDelete, "/api/pipeline-roles/"+role.ID, ""); code != http.StatusBadRequest {
		t.Errorf("delete role in use = %d", code)
	}
	if roles := getText(t, ts, "/api/pipeline-roles"); !strings.Contains(roles, `"pipelines":1`) {
		t.Errorf("roles = %s", roles)
	}

	// 公开：其他人只读
	if code, _ := dev2.do(t, http.MethodPut, base+"/public", `{"public":true}`); code != http.StatusForbidden {
		t.Errorf("non-manager set public = %d", code)
	}
	if code, b := dev1.do(t, http.MethodPut, base+"/public", `{"public":true}`); code != http.StatusOK {
		t.Fatalf("set public = %d %s", code, b)
	}
	if v := getTextAs(t, dev3, base); !strings.Contains(v, `"permissions":["view"]`) {
		t.Errorf("public view = %s", v)
	}
	if code, _ := dev3.do(t, http.MethodPost, base+"/runs", ""); code != http.StatusForbidden {
		t.Errorf("public run = %d", code)
	}
	mem := getTextAs(t, dev3, base+"/members")
	if !strings.Contains(mem, `"role_id":"owner"`) || !strings.Contains(mem, `"username":"dev2"`) {
		t.Errorf("members = %s", mem)
	}

	// 移除成员后又回到只读
	dev1.do(t, http.MethodDelete, base+"/members/"+ids["dev2"], "")
	if code, _ := dev2.do(t, http.MethodPut, base, yaml); code != http.StatusForbidden {
		t.Errorf("removed member edit = %d", code)
	}
}

func getTextAs(t *testing.T, st *stack, path string) string {
	t.Helper()
	code, body := st.do(t, http.MethodGet, path, "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, code, body)
	}
	return body
}

// 账户：资料、会话、API Key 有效期、删除账户。
func TestAccountSelfService(t *testing.T) { storetest.ForEach(t, testAccountSelfService) }

func testAccountSelfService(t *testing.T, s store.Store) {
	ts := newStack(t, s)
	postRaw(t, ts, "/api/members", `{"username":"amy","password":"amy-password","role":"developer"}`, http.StatusCreated)
	amy := ts.as(ts.login(t, "amy", "amy-password"))
	other := ts.as(ts.login(t, "amy", "amy-password"))

	if code, _ := amy.do(t, http.MethodPut, "/api/me/profile", `{"display_name":"艾米","email":"bad"}`); code != http.StatusBadRequest {
		t.Errorf("bad email = %d", code)
	}
	if code, b := amy.do(t, http.MethodPut, "/api/me/profile", `{"display_name":"艾米","email":"amy@example.com","gender":"female","bio":"hi"}`); code != http.StatusOK || !strings.Contains(b, `"email":"amy@example.com"`) {
		t.Errorf("profile = %d %s", code, b)
	}

	var sessions []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	json.Unmarshal([]byte(getTextAs(t, amy, "/api/me/sessions")), &sessions)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v", sessions)
	}
	for _, se := range sessions {
		if !se.Current {
			if code, _ := amy.do(t, http.MethodDelete, "/api/me/sessions/"+se.ID, ""); code != http.StatusNoContent {
				t.Errorf("revoke = %d", code)
			}
		}
	}
	if code, _ := other.do(t, http.MethodGet, "/api/me", ""); code != http.StatusUnauthorized {
		t.Errorf("revoked session = %d", code)
	}

	code, b := amy.do(t, http.MethodPost, "/api/me/tokens", `{"name":"ci","expires_in_days":30}`)
	if code != http.StatusCreated || !strings.Contains(b, `"expires_at"`) {
		t.Errorf("token with expiry = %d %s", code, b)
	}
	if code, _ := amy.do(t, http.MethodPost, "/api/me/tokens", `{"name":"ci","expires_in_days":5000}`); code != http.StatusBadRequest {
		t.Errorf("token expiry too long = %d", code)
	}

	if code, _ := amy.do(t, http.MethodDelete, "/api/me", `{"password":"wrong"}`); code != http.StatusUnauthorized {
		t.Errorf("delete with wrong password = %d", code)
	}
	if code, _ := ts.do(t, http.MethodDelete, "/api/me", `{"password":"`+adminPassword+`"}`); code != http.StatusBadRequest {
		t.Errorf("delete only admin = %d", code)
	}
	if code, _ := amy.do(t, http.MethodDelete, "/api/me", `{"password":"amy-password"}`); code != http.StatusNoContent {
		t.Fatalf("delete account = %d", code)
	}
	if code, _ := amy.do(t, http.MethodGet, "/api/me", ""); code != http.StatusUnauthorized {
		t.Errorf("deleted account = %d", code)
	}
}
