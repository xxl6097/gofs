package gofs_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/xxl6097/gofs"
)

// TestBannerReportsBothNoProgressTimeouts 确认横幅把上传与下载两项
// 「无进展超时」分别打出来。
//
// README 让运维用横幅确认生效的防护值，所以它必须说实话：这两个超时是
// **独立**的开关（--upload-read-timeout / --download-timeout），默认值只是
// 恰好相同（都是 2m）。早先只打 DownloadTimeout 一个、标着笼统的
// 「无进展超时」—— 于是「只改了上传」的运维看到的是下载那个值：
// 要么以为没生效，要么以为下载也跟着改了。
func TestBannerReportsBothNoProgressTimeouts(t *testing.T) {
	srv, err := gofs.New(
		gofs.WithRoot(t.TempDir()),
		gofs.WithPort(0),
		gofs.WithKeyFile(""), gofs.WithUserFile(""), gofs.WithSettingsFile(""),
		gofs.WithTune(func(c *gofs.Config) {
			c.UploadReadTimeout = 30 * time.Second
			c.DownloadTimeout = 90 * time.Second
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var buf bytes.Buffer
	srv.Banner(&buf)
	line := buf.String()
	if !strings.Contains(line, "上传30s") {
		t.Errorf("横幅没打上传侧的超时：\n%s", line)
	}
	if !strings.Contains(line, "下载1m30s") {
		t.Errorf("横幅没打下载侧的超时：\n%s", line)
	}
}

// TestBannerSaysUnlimitedForZeroTimeout 确认 0（不限制）说成「不限」而不是「0s」。
func TestBannerSaysUnlimitedForZeroTimeout(t *testing.T) {
	srv, err := gofs.New(
		gofs.WithRoot(t.TempDir()),
		gofs.WithPort(0),
		gofs.WithKeyFile(""), gofs.WithUserFile(""), gofs.WithSettingsFile(""),
		gofs.WithTune(func(c *gofs.Config) {
			c.DownloadTimeout = 0
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var buf bytes.Buffer
	srv.Banner(&buf)
	if !strings.Contains(buf.String(), "下载不限") {
		t.Errorf("下载不限制时的文案不对：\n%s", buf.String())
	}
}
