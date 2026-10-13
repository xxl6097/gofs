package config

import (
	"os"
	"path/filepath"
	"testing"
)

// withFakeHome 把 HOME 指到一个临时目录。
//
// ⚠️ 整个用例只能调一次：os.UserConfigDir() 每次都读环境变量，多调几次会让
// 前后两次算出来的是不同目录（旧文件在 A、迁移去找 B），看着像迁移没生效。
//
// 这样测试全程不碰开发者真实的配置目录。
func withFakeHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// legacyFile 在「旧位置」造一个文件，返回旧路径与新路径。
func legacyFile(t *testing.T, name string) (string, string) {
	t.Helper()
	old := filepath.Join(LegacyKeyDir(), name)
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte(`{"from":"legacy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return old, filepath.Join(DefaultKeyDir(), name)
}

// TestMigrateMovesLegacyFiles 是这次改动的关键：默认路径多了一层 gofs/，
// 旧位置的文件必须搬过来 —— 否则既有部署的上传密钥与用户表就「没了」。
func TestMigrateMovesLegacyFiles(t *testing.T) {
	withFakeHome(t)
	oldKeys, newKeys := legacyFile(t, "keys.json")
	oldUsers, newUsers := legacyFile(t, "users.json")
	oldSet, newSet := legacyFile(t, "settings.json")

	cfg := Defaults()
	if err := MigrateLegacyDefaults(cfg); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	for _, tc := range []struct{ old, new string }{
		{oldKeys, newKeys}, {oldUsers, newUsers}, {oldSet, newSet},
	} {
		if _, err := os.Stat(tc.new); err != nil {
			t.Errorf("新位置没有文件 %s: %v", tc.new, err)
		}
		if _, err := os.Stat(tc.old); err == nil {
			t.Errorf("旧位置的文件没搬走：%s", tc.old)
		}
	}

	// 内容原样搬过去（不是新建一个空的）。
	b, err := os.ReadFile(newKeys)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"from":"legacy"}` {
		t.Errorf("内容被改动了: %s", b)
	}
}

// TestMigrateIsIdempotent 迁移是启动时跑的，必须能重复调用而不出错、不改内容。
func TestMigrateIsIdempotent(t *testing.T) {
	withFakeHome(t)
	_, newKeys := legacyFile(t, "keys.json")

	cfg := Defaults()
	for i := 0; i < 3; i++ {
		if err := MigrateLegacyDefaults(cfg); err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i+1, err)
		}
	}
	b, err := os.ReadFile(newKeys)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"from":"legacy"}` {
		t.Errorf("重复迁移后内容变了: %s", b)
	}
}

// TestMigrateDoesNotOverwrite 新位置已有文件时绝不覆盖 —— 那可能是当前正在用的。
func TestMigrateDoesNotOverwrite(t *testing.T) {
	withFakeHome(t)
	_, newKeys := legacyFile(t, "keys.json")
	if err := os.MkdirAll(filepath.Dir(newKeys), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newKeys, []byte(`{"from":"new"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := MigrateLegacyDefaults(Defaults()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	b, _ := os.ReadFile(newKeys)
	if string(b) != `{"from":"new"}` {
		t.Errorf("新位置被旧文件覆盖了: %s", b)
	}
}

// TestMigrateSkipsExplicitPaths 显式指定过路径时，不许碰旧目录里的任何东西。
//
// 那种情况下旧位置的文件与本次启动无关，去动它属于越权 —— 尤其不能因为
// 用户把密钥指定到了别处，就把旧目录里的文件也搬走。
func TestMigrateSkipsExplicitPaths(t *testing.T) {
	withFakeHome(t)
	oldKeys, newKeys := legacyFile(t, "keys.json")

	cfg := Defaults()
	cfg.KeyFile = filepath.Join(t.TempDir(), "my-keys.json") // 显式指定
	if err := MigrateLegacyDefaults(cfg); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	if _, err := os.Stat(oldKeys); err != nil {
		t.Errorf("显式指定了路径，却把旧文件搬走了：%s", oldKeys)
	}
	if _, err := os.Stat(newKeys); err == nil {
		t.Errorf("显式指定了路径，却往默认位置写了东西：%s", newKeys)
	}
}

// TestMigrateSkipsMemoryOnly 只存内存（路径为空）时不动任何文件。
func TestMigrateSkipsMemoryOnly(t *testing.T) {
	withFakeHome(t)
	oldKeys, newKeys := legacyFile(t, "keys.json")

	cfg := Defaults()
	cfg.KeyFile = ""
	if err := MigrateLegacyDefaults(cfg); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if _, err := os.Stat(oldKeys); err != nil {
		t.Errorf("只存内存时不该动旧文件：%s", oldKeys)
	}
	if _, err := os.Stat(newKeys); err == nil {
		t.Errorf("只存内存时不该创建新文件：%s", newKeys)
	}
}

// TestMigrateNoopWhenNothingToMove 旧目录为空时什么都不做（也不建新目录）。
func TestMigrateNoopWhenNothingToMove(t *testing.T) {
	withFakeHome(t)

	if err := MigrateLegacyDefaults(Defaults()); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if _, err := os.Stat(DefaultKeyDir()); err == nil {
		t.Errorf("没有旧文件却建了新目录：%s", DefaultKeyDir())
	}
}

// TestDefaultPathsIncludeAppDir 钉住这次的路径改动本身。
func TestDefaultPathsIncludeAppDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := DefaultKeyDir()
	if filepath.Base(dir) != "gofs" {
		t.Errorf("默认目录应当以 gofs 结尾，实际 %s", dir)
	}
	if filepath.Dir(dir) != LegacyKeyDir() {
		t.Errorf("旧目录应当是它上一层：%s vs %s", LegacyKeyDir(), dir)
	}
	// 三个文件都在这一层下面。
	for _, f := range []string{DefaultKeyFile(), DefaultUserFile(), DefaultSettingsFile()} {
		if filepath.Dir(f) != dir {
			t.Errorf("%s 不在 %s 下", f, dir)
		}
	}
}

// TestHomeUnsetFallsBackToAccountHome 是给 systemd 那类场景上的一道保险。
//
// systemd 的**系统级** service 默认不设 HOME。那时 os.UserConfigDir() 与
// os.UserHomeDir() 都会失败，若不兜底，filepath.Join("", "keys.json") 就是
// **相对路径** —— 文件会静默写进服务的 WorkingDirectory（常见是 /），而
// banner 上只显示一个 `keys.json`，使用者看不出它去了哪。
//
// 现在退回账号的真实家目录（查 /etc/passwd，不依赖环境变量），所以拿到的
// 必须是**绝对路径**。
func TestHomeUnsetFallsBackToAccountHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	for _, f := range []string{DefaultKeyFile(), DefaultUserFile(), DefaultSettingsFile()} {
		if !filepath.IsAbs(f) {
			t.Errorf("HOME 未设置时应当回退到账号家目录（绝对路径），实际 %q", f)
		}
		if filepath.Base(filepath.Dir(f)) != "gofs" {
			t.Errorf("%s 不在 gofs/ 之下", f)
		}
	}
	if !filepath.IsAbs(DefaultKeyDir()) {
		t.Errorf("DefaultKeyDir 应当是绝对路径，实际 %q", DefaultKeyDir())
	}

	// 旧目录这时与新的位置相同（旧版当年也拼了 .config/gofs），
	// 于是迁移无事可做 —— 不能反过来把新位置当成旧位置去搬。
	if LegacyKeyDir() != DefaultKeyDir() {
		t.Errorf("HOME 未设置时新旧目录应当相同：legacy=%q new=%q",
			LegacyKeyDir(), DefaultKeyDir())
	}
	if err := MigrateLegacyDefaults(Defaults()); err != nil {
		t.Errorf("迁移不该报错: %v", err)
	}
}
