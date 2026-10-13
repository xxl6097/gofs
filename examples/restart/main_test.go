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
		h, err := restarterFor(mode)
		if err != nil {
			t.Errorf("mode=%s 返回错误: %v", mode, err)
			continue
		}
		// 两个动作都要有：注入一个只有 Restart 的值，页面上就没有「卸载」按钮。
		if h.restart == nil {
			t.Errorf("mode=%s 没有重启动作", mode)
		}
		if h.uninstall == nil {
			t.Errorf("mode=%s 没有卸载动作 —— 页面上不会出现「卸载」按钮", mode)
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

// TestHooksSatisfyBothInterfaces 是「这个值能直接塞进 gofs.WithRestarter，
// 且页面上两个按钮都会出现」这条契约的编译期版本。
//
// 卸载能力是**断言**出来的（cfg.Restarter.(Uninstaller)），所以这个类型必须
// 真的同时满足两个接口 —— 少一个，按钮就静默消失。
func TestHooksSatisfyBothInterfaces(t *testing.T) {
	var _ gofs.Restarter = hooks{}
	var _ gofs.Uninstaller = hooks{}
	// 同时留一条「单独用 RestarterFunc 时**没有**卸载能力」的对照，
	// 免得以后有人把它当成「也支持卸载」。
	var r gofs.Restarter = gofs.RestarterFunc(reexecSelf)
	if _, ok := r.(gofs.Uninstaller); ok {
		t.Error("RestarterFunc 不该凭空获得卸载能力")
	}
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
