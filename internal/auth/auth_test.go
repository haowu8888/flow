package auth

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"flow/internal/model"
	"flow/internal/store"
	"flow/internal/store/storetest"
)

// RFC 7914 第 11 节的 PBKDF2-HMAC-SHA256 测试向量。
func TestPBKDF2Vector(t *testing.T) {
	got := hex.EncodeToString(pbkdf2([]byte("passwd"), []byte("salt"), 1, 64))
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if got != want {
		t.Errorf("pbkdf2 = %s", got)
	}
}

func TestPassword(t *testing.T) {
	h := encodeHash(1000, []byte("0123456789abcdef"), pbkdf2([]byte("correct horse"), []byte("0123456789abcdef"), 1000, 32))
	if !CheckPassword(h, "correct horse") || CheckPassword(h, "wrong") || CheckPassword("garbage", "x") {
		t.Error("CheckPassword mismatch")
	}
}

func TestLoginAndRoles(t *testing.T) {
	storetest.ForEach(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		svc := NewService(s)
		created, generated, err := svc.Bootstrap(ctx, "")
		if err != nil || !created || len(generated) < 8 {
			t.Fatalf("Bootstrap = %v %q %v", created, generated, err)
		}
		if created, _, _ := svc.Bootstrap(ctx, "x"); created {
			t.Fatal("bootstrap ran twice")
		}
		if _, _, err := svc.Login(ctx, "admin", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong password = %v", err)
		}
		if _, _, err := svc.Login(ctx, "nobody", "x"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("unknown user = %v", err)
		}
		tok, admin, err := svc.Login(ctx, "admin", generated)
		if err != nil || admin.Role != model.RoleAdmin || admin.TenantID != "default" {
			t.Fatalf("Login = %+v, %v", admin, err)
		}
		if p, err := svc.Authenticate(ctx, tok); err != nil || p.User.Username != "admin" {
			t.Fatalf("Authenticate = %v", err)
		}
		if _, err := svc.Authenticate(ctx, tok+"x"); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("bad token = %v", err)
		}

		// 管理员添加开发者；开发者无权管理成员。
		if _, err := svc.AddMember(ctx, admin, "bob", "Bob", "", model.RoleDeveloper); !errors.Is(err, ErrInvalid) {
			t.Fatalf("add without password = %v", err)
		}
		if _, err := svc.AddMember(ctx, admin, "bob", "Bob", "short", model.RoleDeveloper); !errors.Is(err, ErrInvalid) {
			t.Fatalf("short password = %v", err)
		}
		bobView, err := svc.AddMember(ctx, admin, "bob", "Bob", "bob-password", model.RoleDeveloper)
		if err != nil {
			t.Fatal(err)
		}
		bobTok, bob, err := svc.Login(ctx, "bob", "bob-password")
		if err != nil || bob.Role != model.RoleDeveloper || !bob.Can(model.RoleViewer) || bob.Can(model.RoleAdmin) {
			t.Fatalf("bob = %+v, %v", bob, err)
		}
		if _, err := svc.AddMember(ctx, bob, "carol", "", "carol-password", model.RoleViewer); !errors.Is(err, ErrForbidden) {
			t.Fatalf("developer adding member = %v", err)
		}
		if err := svc.RemoveMember(ctx, admin, admin.User.ID); !errors.Is(err, ErrInvalid) {
			t.Fatalf("remove self = %v", err)
		}

		// 租户：系统管理员创建，bob 不是成员不能切换。
		if _, err := svc.CreateTenant(ctx, bob, "team-b", "B"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("developer creating tenant = %v", err)
		}
		if _, err := svc.CreateTenant(ctx, admin, "Bad ID", "B"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad tenant id = %v", err)
		}
		if _, err := svc.CreateTenant(ctx, admin, "team-b", "Team B"); err != nil {
			t.Fatal(err)
		}
		if err := svc.SwitchTenant(ctx, bob, "team-b"); !errors.Is(err, ErrForbidden) {
			t.Fatalf("bob switch = %v", err)
		}
		if err := svc.SwitchTenant(ctx, admin, "team-b"); err != nil {
			t.Fatal(err)
		}
		if p, _ := svc.Authenticate(ctx, tok); p.TenantID != "team-b" {
			t.Fatalf("tenant after switch = %s", p.TenantID)
		}
		if ts, _ := svc.Tenants(ctx, bob.User); len(ts) != 1 || ts[0].Role != model.RoleDeveloper {
			t.Fatalf("bob tenants = %+v", ts)
		}

		// 改密码后旧会话失效；API 令牌不受影响且可删除。
		apiPlain, apiTok, err := svc.CreateAPIToken(ctx, bob, "ci", 0)
		if err != nil || apiTok.Prefix[:3] != "fp_" {
			t.Fatalf("api token = %+v, %v", apiTok, err)
		}
		_, bob2, _ := svc.Login(ctx, "bob", "bob-password")
		if err := svc.ChangePassword(ctx, bob2, "wrong", "new-password-1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("change with wrong old = %v", err)
		}
		if err := svc.ChangePassword(ctx, bob2, "bob-password", "new-password-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authenticate(ctx, bobTok); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("old session after password change = %v", err)
		}
		if _, err := svc.Authenticate(ctx, apiPlain); err != nil {
			t.Fatalf("api token after password change = %v", err)
		}
		if err := svc.DeleteAPIToken(ctx, admin, apiTok.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("delete other's token = %v", err)
		}
		if err := svc.DeleteAPIToken(ctx, bob2, apiTok.ID); err != nil {
			t.Fatal(err)
		}
		if err := svc.SetMemberRole(ctx, admin, bobView.UserID, model.RoleViewer); err != nil {
			t.Fatal(err)
		}
		if _, p, _ := svc.Login(ctx, "bob", "new-password-1"); p.Role != model.RoleViewer {
			t.Fatalf("role after change = %s", p.Role)
		}
		admin, _ = svc.Authenticate(ctx, tok) // 会话已切到 team-b，bob 不是其成员
		if err := svc.RemoveMember(ctx, admin, bobView.UserID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("remove non-member = %v", err)
		}
	})
}
