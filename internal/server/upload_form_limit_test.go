package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMultipartFormHasHardCap 确认表单上传有独立于流式 PUT 的硬上限。
//
// multipart 超过 maxFormMemory 的部分会落到系统临时目录；若沿用
// UploadMaxSize（默认 10 GiB，0 表示不限），单个请求就能写满 /tmp。
func TestMultipartFormHasHardCap(t *testing.T) {
	root := makeFixture(t)
	// 上传上限显式设为「不限制」，表单路径仍必须有自己的 1 GiB 上限。
	s := newTestServer(t, testConfig(t, root, "-A", "--upload-max-size", "0"))

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	r.ContentLength = maxFormBody + 1
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "PUT") {
		t.Errorf("413 文案没有提示改用 PUT：%s", w.Body.String())
	}
}
