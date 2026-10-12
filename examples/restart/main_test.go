package main

import (
	"os"
	"testing"

	"github.com/xxl6097/gofs"
)

// TestRestarterFor 覆盖策略选择。
//
// 选错策略的后果是「点了没反应」或「服务直接停掉」，都要手工点一次
// 才看得出来，所以这里把三个分支连同未知取值都钉住。
func TestRestarterFor(t *testing.T) {
	for _, mode := range []string{"exec", "signal"} {
		r, err := restarterFor(mode)
		if err != nil {
			t.Errorf("mode=%s 返回错误: %v", mode, err)
		}
		if r == nil {
			t.Errorf("mode=%s 返回了 nil —— 注入 nil 等于没注入，按钮会消失", mode)
		}
	}

	// 未知取值必须报错，而不是悄悄退化成某个默认策略。
	if _, err := restarterFor("reexec"); err == nil {
		t.Error("未知的 mode 应当报错")
	}
	if _, err := restarterFor(""); err == nil {
		t.Error("空 mode 应当报错")
	}
}

// TestStrategiesSatisfyRestarter 是「两种策略都能直接塞进
// gofs.WithRestarter」这条契约的运行时版本。
func TestStrategiesSatisfyRestarter(t *testing.T) {
	var _ gofs.Restarter = gofs.RestarterFunc(reexecSelf)
	var _ gofs.Restarter = gofs.RestarterFunc(signalSelf)
}

// TestExecTargetPointsAtThisBinary 覆盖 reexecSelf 里唯一能安全测的部分。
//
// ⚠️ 不能直接调 reexecSelf：syscall.Exec 一旦成功，跑测试的这个进程就被
// 换掉了，后面的断言永远不会执行。所以把「重执行谁、带什么参数」抽成
// execTarget 单独测 —— 这正是它被抽出来的原因。
func TestExecTargetPointsAtThisBinary(t *testing.T) {
	exe, argv, err := execTarget()
	if err != nil {
		t.Fatalf("execTarget: %v", err)
	}
	if exe == "" {
		t.Fatal("可执行文件路径是空的 —— 重执行换不成任何东西")
	}
	// 必须是真实存在的文件。`go run` 编译出来的临时文件就属于「看起来有
	// 路径、其实随时会消失」，这也是示例里反复强调别用 go run 的原因。
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("os.Executable() 给出的路径不存在: %v", err)
	}
	if st.IsDir() {
		t.Fatalf("%s 是目录", exe)
	}
	// 参数要原样带过去：启动时用的是哪些 flag、服务哪个目录，重启后就该
	// 还是哪些 —— 否则「重启」会变成「换个配置启动」。
	if len(argv) == 0 || argv[0] != os.Args[0] {
		t.Errorf("参数没沿用本进程：%v，期望首项 %q", argv, os.Args[0])
	}
}

// TestSignalSelfIsNotCalled 说明为什么这里**没有** signalSelf 的测试。
//
// 它唯一的动作就是给自己发 SIGTERM —— 调用它等于把跑测试的进程杀掉，
// 换 SIGNAL 之类「安全的信号」又变成在测 syscall.Kill 而不是测这段代码。
// 这类「一条语句、语义就是自毁」的实现，测试的收益比不上它带来的风险；
// 真正要守的边界（谁能触发、未注入时如何）在 internal/server 的
// restart_test.go 里已经覆盖了。
func TestSignalSelfIsNotCalled(t *testing.T) {
	// 只做静态确认：它确实是个合法的 Restarter，且不是 nil。
	var r gofs.Restarter = gofs.RestarterFunc(signalSelf)
	if r == nil {
		t.Fatal("signalSelf 不是合法的 Restarter")
	}
}
