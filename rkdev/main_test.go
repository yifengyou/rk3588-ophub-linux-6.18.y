package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func doUpload(t *testing.T, filename string, data []byte, target string) (int, map[string]string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("firmware", filename)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	mw.WriteField("target_device", target)
	mw.Close()

	req := httptest.NewRequest("POST", "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	handleUpload(rec, req)

	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// stagedFile 返回 /upload 暂存目录中唯一 fw_* 文件的内容
func stagedFile(t *testing.T) (string, []byte) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(uploadDir, "fw_*"))
	if len(matches) != 1 {
		t.Fatalf("expect 1 staged file, got %v", matches)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return matches[0], data
}

func TestUploadMatrix(t *testing.T) {
	uploadDir = t.TempDir()
	raw := bytes.Repeat([]byte("BDY-G98-FIRMWARE-"), 4096) // 64KB+

	cases := []struct {
		name        string
		filename    string
		data        []byte
		target      string
		wantCode    int
		wantMsg     string // 响应中应包含的子串
		wantContent []byte // 非 nil 时校验暂存文件内容
		wantSuffix  string // 非空时校验暂存文件名后缀
	}{
		{"raw img", "debian.img", raw, "/dev/null_test", 200, "已暂存", raw, ".img"},
		{"gzip img", "debian.img.gz", gzipData(t, raw), "/dev/null_test", 200, "已自动解压", raw, ".img"},
		{"gzip upper ext", "OPENWRT.BIN.GZ", gzipData(t, raw), "/dev/null_test", 200, "已自动解压", raw, ".BIN"},
		{"raw named gz", "fake.img.gz", raw, "/dev/null_test", 200, "已暂存", raw, ".img"},
		{"unsupported txt", "notes.txt", raw, "/dev/null_test", 400, "不支持的文件类型", nil, ""},
		{"gz no inner ext", "archive.gz", gzipData(t, raw), "/dev/null_test", 400, "不支持的文件类型", nil, ""},
		{"truncated gzip", "broken.img.gz", gzipData(t, raw)[:100], "/dev/null_test", 400, "gzip 解压失败", nil, ""},
		{"bad target", "debian.img", raw, "sda", 400, "非法目标设备路径", nil, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, resp := doUpload(t, c.filename, c.data, c.target)
			if code != c.wantCode {
				t.Fatalf("code=%d want=%d resp=%v", code, c.wantCode, resp)
			}
			combined := resp["error"] + resp["message"]
			if !strings.Contains(combined, c.wantMsg) {
				t.Fatalf("resp %q does not contain %q", combined, c.wantMsg)
			}
			if c.wantContent != nil {
				path, content := stagedFile(t)
				if !bytes.Equal(content, c.wantContent) {
					t.Fatalf("staged content mismatch: got %d bytes want %d", len(content), len(c.wantContent))
				}
				if c.wantSuffix != "" && !strings.HasSuffix(path, c.wantSuffix) {
					t.Fatalf("staged name %q want suffix %q", path, c.wantSuffix)
				}
				os.Remove(path)
			}
		})
	}
}
