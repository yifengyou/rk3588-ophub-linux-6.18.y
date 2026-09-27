package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/creack/pty"
	"golang.org/x/net/websocket"
)

//go:embed index.html
var indexHTML string

//go:embed res
var resFS embed.FS

var uploadDir = "/tmp"
const blockSize = 4 * 1024 * 1024

// ============ JSON Helpers ============

func jsonOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ============ Flash Task Management ============

// FlashWriteItem 是一次"把某个文件写到目标设备某个字节偏移处"的任务。
// 单镜像模式下只有一个 Item（Offset=0，整盘写入）；
// 多文件（parameter.txt + config.cfg）模式下每个分区一个 Item。
type FlashWriteItem struct {
	Name   string // 分区名 / 显示名，仅用于日志和进度展示
	Offset int64  // 在目标设备上的字节偏移
	Size   int64  // 源文件大小（字节），用于进度计算
	Path   string // 源文件在本地磁盘上的路径
}

type FlashTask struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Written  int64   `json:"written"`
	Total    int64   `json:"total"`
	Speed    float64 `json:"speed"`
	Error    string  `json:"error,omitempty"`
	Mode     string  `json:"mode,omitempty"` // "single" 或 "multi"，仅供参考

	Items        []FlashWriteItem `json:"-"`
	TargetDev    string           `json:"-"`
	CleanupPaths []string         `json:"-"` // 完成后需要删除的临时文件/目录
}

var (
	tasks   = make(map[string]*FlashTask)
	tasksMu sync.RWMutex
	taskSeq int
)

func newTask(target string, total int64) *FlashTask {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	taskSeq++
	id := fmt.Sprintf("task_%d_%d", time.Now().UnixNano(), taskSeq)
	t := &FlashTask{ID: id, Status: "writing", Total: total, TargetDev: target}
	tasks[id] = t
	return t
}

func getTask(id string) *FlashTask {
	tasksMu.RLock()
	defer tasksMu.RUnlock()
	return tasks[id]
}

func updateTask(id string, fn func(*FlashTask)) {
	tasksMu.Lock()
	defer tasksMu.Unlock()
	if t, ok := tasks[id]; ok {
		fn(t)
	}
}

// ============ Block Device Structures ============

type BlockDevice struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  string `json:"size"`
	Model string `json:"model"`
}

type LsblkOutput struct {
	BlockDevices []LsblkDevice `json:"blockdevices"`
}

// 【关键修复】
// Size 用 json.Number：兼容 lsblk 输出数字(128035675648)或字符串("128035675648")两种格式
// Model/Tran 用 *string 指针：兼容 null 值（虽然 string 也能接收 null，但指针更明确）
type LsblkDevice struct {
	Name  string      `json:"name"`
	Size  json.Number `json:"size"`
	Type  string      `json:"type"`
	Model *string     `json:"model"`
	Tran  *string     `json:"tran"`
}

// ============ Main ============

func main() {
	tmpl := template.Must(template.New("index").Parse(indexHTML))

	resSubFS, err := fs.Sub(resFS, "res")
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { tmpl.Execute(w, nil) })
	http.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) { jsonOK(w, getFilteredDevices()) })
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/api/progress", handleProgress)
	http.HandleFunc("/api/reboot", handleReboot)

	// ✅ WebSocket 终端端点
	http.Handle("/api/terminal", websocket.Handler(handleTerminal))
	http.Handle("/res/", http.StripPrefix("/res/", http.FileServer(http.FS(resSubFS))))

	fmt.Println("========================================")
	fmt.Println("  RKdev + Terminal port:80 protocol:http")
	fmt.Println("========================================")
	log.Fatal(http.ListenAndServe(":80", nil))
}

// ============ Terminal (PTY over WebSocket) ============

func handleTerminal(ws *websocket.Conn) {
	defer ws.Close()

	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}

	cmd := exec.Command(shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[TERM] PTY start failed: %v", err)
		ws.Write([]byte("\r\n*** Failed to start terminal: " + err.Error() + " ***\r\n"))
		return
	}
	defer ptmx.Close()

	pty.Setsize(ptmx, &pty.Winsize{Rows: 30, Cols: 120})

	done := make(chan struct{}, 2)

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		for {
			n, err := ws.Read(buf)
			if err != nil {
				return
			}
			if n > 0 && buf[0] == '0' {
				ptmx.Write(buf[1:n])
			} else if n >= 5 && buf[0] == '1' {
				rows := uint16(buf[1])<<8 | uint16(buf[2])
				cols := uint16(buf[3])<<8 | uint16(buf[4])
				pty.Setsize(ptmx, &pty.Winsize{Rows: rows, Cols: cols})
			}
		}
	}()

	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				return
			}
			if _, werr := ws.Write(buf[:n]); werr != nil {
				return
			}
		}
	}()

	<-done
	cmd.Process.Kill()
	cmd.Wait()
	log.Println("[TERM] Session ended")
}

// ============ Reboot ============

func handleReboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	log.Println("[REBOOT] Requested")
	jsonOK(w, map[string]string{"message": "系统即将重启..."})
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("/sbin/reboot").Run()
	}()
}

// ============ Upload & Flash ============

// checkFirmwareName 校验固件文件名，返回解压后的逻辑文件名
// 支持 xxx.img / xxx.bin 及其 gzip 压缩形式 xxx.img.gz / xxx.bin.gz（大小写不敏感）
func checkFirmwareName(name string) (string, error) {
	lower := strings.ToLower(name)
	inner := lower
	if strings.HasSuffix(inner, ".gz") {
		inner = inner[:len(inner)-len(".gz")]
	}
	if strings.HasSuffix(inner, ".img") || strings.HasSuffix(inner, ".bin") {
		return name[:len(inner)], nil
	}
	return "", fmt.Errorf("不支持的文件类型 %q，仅支持 .img / .bin 固件、其 gzip 压缩包(.img.gz / .bin.gz)，或 .zip 固件包", name)
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	// MultipartReader 流式接收。
	// - 单个 .img/.bin(.gz)：像原来一样边收边解压，压缩包不整体落盘。
	// - .zip 固件包：需要随机访问才能解压，只能先整体落盘到 /tmp，再解压分析。
	mr, err := r.MultipartReader()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "解析表单失败: "+err.Error())
		return
	}

	var target, safeName, innerName, tmpPath, zipPath string
	var written int64
	var isGzip, isZip bool

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "读取表单失败: "+err.Error())
			return
		}

		switch part.FormName() {
		case "target_device":
			b, _ := io.ReadAll(io.LimitReader(part, 1024))
			target = strings.TrimSpace(string(b))

		case "firmware":
			safeName = filepath.Base(part.FileName())
			if safeName == "" || safeName == "." {
				jsonErr(w, http.StatusBadRequest, "获取文件失败: 文件名为空")
				return
			}
			lowerName := strings.ToLower(safeName)

			if strings.HasSuffix(lowerName, ".zip") {
				// ---- 情况：固件包 (.zip)，里面可能是 parameter.txt + config.cfg + 多个镜像 ----
				isZip = true
				zipPath = filepath.Join(uploadDir, fmt.Sprintf("pkg_%d.zip", time.Now().UnixNano()))
				dst, cerr := os.Create(zipPath)
				if cerr != nil {
					jsonErr(w, http.StatusInternalServerError, "创建临时文件失败: "+cerr.Error())
					return
				}
				written, err = io.Copy(dst, part)
				dst.Close()
				if err != nil {
					os.Remove(zipPath)
					jsonErr(w, http.StatusInternalServerError, "保存固件包失败: "+err.Error())
					return
				}
				if written == 0 {
					os.Remove(zipPath)
					jsonErr(w, http.StatusBadRequest, "文件内容为空")
					return
				}
			} else {
				// ---- 情况：单一镜像文件 .img/.bin，或其 gzip 压缩形式 ----
				innerName, err = checkFirmwareName(safeName)
				if err != nil {
					jsonErr(w, http.StatusBadRequest, err.Error())
					return
				}

				// 按魔数识别 gzip 内容（不看扩展名），是 gzip 则流式解压
				head := make([]byte, 2)
				n, _ := io.ReadFull(part, head)
				head = head[:n]
				isGzip = n == 2 && head[0] == 0x1f && head[1] == 0x8b

				var src io.Reader = io.MultiReader(bytes.NewReader(head), part)
				if isGzip {
					gz, gerr := gzip.NewReader(src)
					if gerr != nil {
						jsonErr(w, http.StatusBadRequest, "gzip 解压失败: "+gerr.Error())
						return
					}
					defer gz.Close()
					src = gz
				}

				tmpPath = filepath.Join(uploadDir, fmt.Sprintf("fw_%d_%s", time.Now().UnixNano(), innerName))
				dst, cerr := os.Create(tmpPath)
				if cerr != nil {
					jsonErr(w, http.StatusInternalServerError, "创建临时文件失败: "+cerr.Error())
					return
				}
				written, err = io.Copy(dst, src)
				dst.Close()
				if err != nil {
					os.Remove(tmpPath)
					if isGzip {
						jsonErr(w, http.StatusBadRequest, "gzip 解压失败: "+err.Error())
					} else {
						jsonErr(w, http.StatusInternalServerError, "保存文件失败: "+err.Error())
					}
					return
				}
				if written == 0 {
					os.Remove(tmpPath)
					jsonErr(w, http.StatusBadRequest, "文件内容为空")
					return
				}
			}
		}
		part.Close()
	}

	if !isZip && tmpPath == "" {
		jsonErr(w, http.StatusBadRequest, "获取文件失败: 缺少 firmware 文件")
		return
	}
	if target == "" || !strings.HasPrefix(target, "/dev/") || strings.Contains(target, "..") {
		if isZip {
			os.Remove(zipPath)
		} else {
			os.Remove(tmpPath)
		}
		jsonErr(w, http.StatusBadRequest, "非法目标设备路径")
		return
	}

	var task *FlashTask
	var msg string

	if isZip {
		// 解压固件包，再判断"解压后是单一镜像"还是"多个文件"
		extractDir := filepath.Join(uploadDir, fmt.Sprintf("pkg_%d", time.Now().UnixNano()))
		if err := os.MkdirAll(extractDir, 0755); err != nil {
			os.Remove(zipPath)
			jsonErr(w, http.StatusInternalServerError, "创建解压目录失败: "+err.Error())
			return
		}
		files, err := extractZip(zipPath, extractDir)
		os.Remove(zipPath) // zip 本身解压完就不再需要
		if err != nil {
			os.RemoveAll(extractDir)
			jsonErr(w, http.StatusBadRequest, "解压固件包失败: "+err.Error())
			return
		}

		task, msg, err = buildTaskFromExtracted(files, extractDir, target)
		if err != nil {
			os.RemoveAll(extractDir)
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("[UPLOAD] %s -> 已解压到 %s，共 %d 个文件，mode=%s -> %s",
			safeName, extractDir, len(files), task.Mode, target)
	} else {
		task = newTask(target, written)
		task.Mode = "single"
		task.Items = []FlashWriteItem{{Name: innerName, Offset: 0, Size: written, Path: tmpPath}}
		task.CleanupPaths = []string{tmpPath}

		msg = fmt.Sprintf("文件 %s (%d MB) 已暂存，正在刷写到 %s ...", safeName, written/1024/1024, target)
		if isGzip {
			msg = fmt.Sprintf("gzip 固件 %s 已自动解压为 %s (%d MB)，正在刷写到 %s ...", safeName, innerName, written/1024/1024, target)
		}
		log.Printf("[UPLOAD] %s -> %s (%d bytes, gzip=%v) -> %s", safeName, tmpPath, written, isGzip, target)
	}

	go doFlash(task)

	jsonOK(w, map[string]string{
		"task_id": task.ID,
		"message": msg,
	})
}

// extractZip 把固件包解压到 destDir，返回所有被解出的常规文件的绝对路径（保留原有目录结构，
// 方便 config.cfg 里 "Image/xxx.img" 这类相对路径能正确解析）。
func extractZip(zipPath, destDir string) ([]string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var files []string
	for _, f := range r.File {
		// 防止 zip slip（../ 或绝对路径逃逸出 destDir）
		cleanName := filepath.Clean(f.Name)
		if cleanName == ".." || strings.HasPrefix(cleanName, "../") || filepath.IsAbs(cleanName) {
			log.Printf("extractZip: 跳过可疑路径条目: %s", f.Name)
			continue
		}
		outPath := filepath.Join(destDir, cleanName)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(outPath, 0755); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return nil, err
		}

		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		out, err := os.Create(outPath)
		if err != nil {
			rc.Close()
			return nil, err
		}
		_, err = io.Copy(out, rc)
		out.Close()
		rc.Close()
		if err != nil {
			return nil, err
		}
		files = append(files, outPath)
	}
	return files, nil
}

// findByBaseName 在解压出的文件列表中按文件名（不含路径，大小写不敏感）查找。
func findByBaseName(files []string, name string) string {
	for _, f := range files {
		if strings.EqualFold(filepath.Base(f), name) {
			return f
		}
	}
	return ""
}

// buildTaskFromExtracted 决定固件包该怎么刷：
//   - 解压后只有一个 .img/.bin 镜像 -> 视为整盘镜像，直接从设备起始位置写入（跟单文件上传一致）。
//   - 解压后有多个 .img/.bin 镜像   -> 必须有 parameter.txt 提供分区表；
//     再尝试用 config.cfg 里的 name->path 映射为每个分区找到对应镜像，
//     找不到的分区用文件名做兜底猜测（<part_name>.img 等）。
func buildTaskFromExtracted(files []string, extractDir, target string) (*FlashTask, string, error) {
	var imageFiles []string
	for _, f := range files {
		ext := strings.ToLower(filepath.Ext(f))
		if ext == ".img" || ext == ".bin" {
			imageFiles = append(imageFiles, f)
		}
	}

	if len(imageFiles) == 0 {
		return nil, "", fmt.Errorf("固件包中未找到任何 .img/.bin 镜像文件")
	}

	// ---- 情况一：解压后只有单一镜像 -> 直接整盘刷入 ----
	if len(imageFiles) == 1 {
		st, err := os.Stat(imageFiles[0])
		if err != nil {
			return nil, "", fmt.Errorf("读取镜像文件失败: %v", err)
		}
		task := newTask(target, st.Size())
		task.Mode = "single"
		task.Items = []FlashWriteItem{{Name: filepath.Base(imageFiles[0]), Offset: 0, Size: st.Size(), Path: imageFiles[0]}}
		task.CleanupPaths = []string{extractDir}
		msg := fmt.Sprintf("固件包内只有单一镜像 %s (%d MB)，将整盘写入 %s ...",
			filepath.Base(imageFiles[0]), st.Size()/1024/1024, target)
		return task, msg, nil
	}

	// ---- 情况二：多个文件 -> 按 parameter.txt 的分区表 + config.cfg 的镜像映射分别写入 ----
	paramPath := findByBaseName(files, "parameter.txt")
	if paramPath == "" {
		return nil, "", fmt.Errorf("固件包内有 %d 个镜像文件，但未找到 parameter.txt，无法确定各镜像应写入哪个分区", len(imageFiles))
	}
	partitions, err := parseParameterFile(paramPath)
	if err != nil {
		return nil, "", fmt.Errorf("解析 parameter.txt 失败: %v", err)
	}

	var cfgItems []CfgImageItem
	cfgPath := findByBaseName(files, "config.cfg")
	if cfgPath != "" {
		if isConfigCfg(cfgPath) {
			cfgItems, err = parseConfigCfg(cfgPath)
			if err != nil {
				log.Printf("解析 config.cfg 失败，将回退到按文件名猜测镜像: %v", err)
				cfgItems = nil
			}
		} else {
			log.Printf("config.cfg 内容不是有效的 CFG 格式，将回退到按文件名猜测镜像")
		}
	} else {
		log.Printf("固件包内未找到 config.cfg，将按文件名猜测每个分区对应的镜像")
	}

	var items []FlashWriteItem
	var matchedNames []string
	var skipped []string

	for _, p := range partitions {
		var imgPath string
		var ok bool

		if cfgItems != nil {
			imgPath, ok = findImageInCfg(cfgItems, files, extractDir, p.Name)
		}
		if !ok {
			imgPath, ok = findImageByGuess(files, p.Name)
		}
		if !ok {
			skipped = append(skipped, p.Name)
			continue
		}

		st, err := os.Stat(imgPath)
		if err != nil {
			skipped = append(skipped, p.Name)
			continue
		}
		if p.Size != 0xFFFFFFFF {
			partBytes := int64(p.Size) * 512
			if st.Size() > partBytes {
				log.Printf("警告: 分区 '%s' 镜像 %s (%d 字节) 大于分区容量 (%d 字节)，仍尝试写入",
					p.Name, imgPath, st.Size(), partBytes)
			}
		}

		items = append(items, FlashWriteItem{
			Name:   p.Name,
			Offset: int64(p.Offset) * 512,
			Size:   st.Size(),
			Path:   imgPath,
		})
		matchedNames = append(matchedNames, p.Name)
	}

	if len(items) == 0 {
		return nil, "", fmt.Errorf("parameter.txt 中的 %d 个分区均未找到对应镜像文件", len(partitions))
	}

	var total int64
	for _, it := range items {
		total += it.Size
	}

	task := newTask(target, total)
	task.Mode = "multi"
	task.Items = items
	task.CleanupPaths = []string{extractDir}

	msg := fmt.Sprintf("解析完成: 将写入分区 [%s]", strings.Join(matchedNames, ", "))
	if len(skipped) > 0 {
		msg += fmt.Sprintf("；未找到镜像已跳过: [%s]", strings.Join(skipped, ", "))
	}
	return task, msg, nil
}

// doFlash 依次把 task.Items 里的每个文件写到 task.TargetDev 对应的字节偏移处，
// 并按累计写入字节数 / task.Total 汇报整体进度。单镜像模式下 Items 只有一项(Offset=0)，
// 效果与原来的"整盘 dd"完全一致。
func doFlash(task *FlashTask) {
	defer cleanupTask(task)

	log.Printf("[%s] Flash start: mode=%s, %d item(s) -> %s (%d bytes total)",
		task.ID, task.Mode, len(task.Items), task.TargetDev, task.Total)

	dst, err := os.OpenFile(task.TargetDev, os.O_WRONLY|os.O_SYNC, 0)
	if err != nil {
		updateTask(task.ID, func(t *FlashTask) { t.Status = "error"; t.Error = "打开目标设备失败: " + err.Error() })
		return
	}
	defer dst.Close()

	buf := make([]byte, blockSize)
	var totalWritten int64
	start := time.Now()

	for _, item := range task.Items {
		src, err := os.Open(item.Path)
		if err != nil {
			updateTask(task.ID, func(t *FlashTask) {
				t.Status = "error"
				t.Error = fmt.Sprintf("打开源文件 %s 失败: %v", item.Name, err)
			})
			return
		}

		if _, err := dst.Seek(item.Offset, io.SeekStart); err != nil {
			src.Close()
			updateTask(task.ID, func(t *FlashTask) {
				t.Status = "error"
				t.Error = fmt.Sprintf("定位分区 '%s' (offset=%d) 失败: %v", item.Name, item.Offset, err)
			})
			return
		}

		log.Printf("[%s] Writing '%s' (%d bytes) @ byte offset %d", task.ID, item.Name, item.Size, item.Offset)

		for {
			n, readErr := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					src.Close()
					updateTask(task.ID, func(t *FlashTask) {
						t.Status = "error"
						t.Error = fmt.Sprintf("写入分区 '%s' 失败 @ 累计 %d 字节: %v", item.Name, totalWritten, werr)
					})
					return
				}
				totalWritten += int64(n)
				elapsed := time.Since(start).Seconds()
				updateTask(task.ID, func(t *FlashTask) {
					t.Written = totalWritten
					if t.Total > 0 {
						t.Progress = float64(totalWritten) / float64(t.Total) * 100
					}
					if elapsed > 0 {
						t.Speed = float64(totalWritten) / elapsed
					}
				})
			}
			if readErr != nil {
				if readErr != io.EOF {
					src.Close()
					updateTask(task.ID, func(t *FlashTask) {
						t.Status = "error"
						t.Error = fmt.Sprintf("读取 '%s' 失败: %v", item.Name, readErr)
					})
					return
				}
				break
			}
		}
		src.Close()
	}

	updateTask(task.ID, func(t *FlashTask) { t.Status = "syncing" })
	dst.Sync()

	elapsed := time.Since(start).Seconds()
	updateTask(task.ID, func(t *FlashTask) {
		t.Status = "done"
		t.Progress = 100
		t.Written = t.Total
		if elapsed > 0 {
			t.Speed = float64(t.Total) / elapsed
		}
	})
	log.Printf("[%s] ✅ Done: %d bytes total, %.1fs, %.1f MB/s",
		task.ID, totalWritten, elapsed, float64(totalWritten)/elapsed/1024/1024)
}

func cleanupTask(task *FlashTask) {
	for _, p := range task.CleanupPaths {
		os.RemoveAll(p)
	}
}

// ============ parameter.txt parsing ============
// 只关心分区表 (mtdparts)，格式: mtdparts=...:SIZE@OFFSET(NAME)[,SIZE@OFFSET(NAME)]*
// SIZE/OFFSET 都是十六进制扇区数，SIZE 为 "-" 表示占满剩余空间（对应 0xFFFFFFFF）。

type ParamPartition struct {
	Name   string
	Offset uint32 // 单位: 扇区 (512 字节)
	Size   uint32 // 单位: 扇区；0xFFFFFFFF 表示到盘尾（grow）
}

func parseParameterFile(path string) ([]ParamPartition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseParameterBytes(string(data))
}

func parseParameterBytes(content string) ([]ParamPartition, error) {
	var result []ParamPartition
	content = strings.ReplaceAll(content, "\r\n", "\n")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, "mtdparts")
		if idx < 0 {
			continue
		}
		colonIdx := strings.Index(line[idx:], ":")
		if colonIdx < 0 {
			continue
		}
		partsStr := line[idx+colonIdx+1:]
		for _, entry := range strings.Split(partsStr, ",") {
			if p, ok := parsePartitionEntry(entry); ok {
				result = append(result, p)
			}
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("未在文件中找到 mtdparts 分区信息")
	}
	return result, nil
}

func parsePartitionEntry(entry string) (ParamPartition, bool) {
	entry = strings.TrimSpace(entry)
	atIdx := strings.Index(entry, "@")
	if atIdx < 0 {
		return ParamPartition{}, false
	}
	sizeStr := strings.TrimSpace(entry[:atIdx])
	rest := entry[atIdx+1:]

	parenIdx := strings.Index(rest, "(")
	if parenIdx < 0 {
		return ParamPartition{}, false
	}
	offsetStr := strings.TrimSpace(rest[:parenIdx])
	rest2 := rest[parenIdx+1:]

	closeIdx := strings.Index(rest2, ")")
	if closeIdx < 0 {
		return ParamPartition{}, false
	}
	name := strings.TrimSpace(rest2[:closeIdx])
	if name == "" {
		return ParamPartition{}, false
	}

	var size uint32
	if strings.Contains(sizeStr, "-") {
		size = 0xFFFFFFFF
	} else {
		v, err := parseHex32(sizeStr)
		if err != nil {
			return ParamPartition{}, false
		}
		size = v
	}

	offset, err := parseHex32(offsetStr)
	if err != nil {
		return ParamPartition{}, false
	}

	return ParamPartition{Name: name, Offset: offset, Size: size}, true
}

func parseHex32(s string) (uint32, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0x")
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// ============ config.cfg parsing (RKDevTool 固件下载配置) ============
// 二进制格式：
//   header(29字节): magic"CFG"(4,第4字节为\0) + gap0(18,未知) + length(1,未知) +
//                   begin(4,LE,第一条记录的绝对偏移) + itemSize(2,LE,每条记录的真实字节长度)
//   record: size(2,LE) + name(UTF16LE,40 units) + path(UTF16LE,260 units) +
//           address(4,LE) + isSelected(1) [+ 记录尾部可能有版本相关的多余字段]
// 解析时严格按 header 声明的 itemSize 逐条切片，而不是假设固定长度，
// 这样不同版本 RKDevTool 生成的记录长度不同也不会错位。

const (
	cfgHeaderSize  = 29 // 4 + 18 + 1 + 4 + 2
	cfgNameUnits   = 40
	cfgPathUnits   = 260
	cfgMinItemSize = 2 + cfgNameUnits*2 + cfgPathUnits*2 + 4 + 1
	cfgAddrAuto    = 0xFFFFFFFF
)

type CfgImageItem struct {
	Name     string
	Path     string // 已把 '\' 换成 '/'
	Address  uint32 // 烧录地址；0xFFFFFFFF 表示 AUTO
	Selected bool
}

// isConfigCfg 只看 magic，快速判断一个文件是不是 config.cfg。
func isConfigCfg(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	return n == 4 && string(magic[:3]) == "CFG"
}

func parseConfigCfg(path string) ([]CfgImageItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < cfgHeaderSize {
		return nil, fmt.Errorf("文件太小，读不到完整 header")
	}
	if string(data[0:3]) != "CFG" {
		return nil, fmt.Errorf("magic 不匹配，不是 config.cfg")
	}

	begin := binary.LittleEndian.Uint32(data[23:27])
	itemSize := binary.LittleEndian.Uint16(data[27:29])

	if itemSize < cfgMinItemSize {
		return nil, fmt.Errorf("header.itemSize=%d 过小（至少需要 %d 字节），文件可能已损坏或版本不同",
			itemSize, cfgMinItemSize)
	}
	if int(begin) < cfgHeaderSize || int(begin) > len(data) {
		return nil, fmt.Errorf("header.begin=0x%X 超出文件范围（文件大小=%d）", begin, len(data))
	}

	payload := len(data) - int(begin)
	entries := payload / int(itemSize)
	if remainder := payload % int(itemSize); remainder != 0 {
		log.Printf("parseConfigCfg: 警告，末尾多出 %d 字节，不是 itemSize 的整数倍", remainder)
	}

	items := make([]CfgImageItem, 0, entries)
	off := int(begin)
	for i := 0; i < entries; i++ {
		if off+int(itemSize) > len(data) {
			log.Printf("parseConfigCfg: item %d 越界，提前结束", i)
			break
		}
		rec := data[off : off+int(itemSize)]
		off += int(itemSize)

		selfSize := binary.LittleEndian.Uint16(rec[0:2])
		if selfSize != itemSize {
			log.Printf("parseConfigCfg: 警告，item %d 自身 size(%d) 与 header.itemSize(%d) 不一致",
				i, selfSize, itemSize)
		}

		nameBytes := rec[2 : 2+cfgNameUnits*2]
		pathBytes := rec[2+cfgNameUnits*2 : 2+cfgNameUnits*2+cfgPathUnits*2]
		addrOff := 2 + cfgNameUnits*2 + cfgPathUnits*2
		address := binary.LittleEndian.Uint32(rec[addrOff : addrOff+4])
		selected := rec[addrOff+4] != 0

		name := utf16leToString(nameBytes)
		p := strings.ReplaceAll(utf16leToString(pathBytes), "\\", "/")

		items = append(items, CfgImageItem{Name: name, Path: p, Address: address, Selected: selected})
	}
	return items, nil
}

// utf16leToString 把 UTF16LE 字节数组（以 0x0000 结尾或读满整个切片）转成 Go string，
// unicode/utf16.Decode 自动处理代理对(surrogate pair)。
func utf16leToString(b []byte) string {
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		v := uint16(b[i]) | uint16(b[i+1])<<8
		if v == 0 {
			break
		}
		u16 = append(u16, v)
	}
	return string(utf16.Decode(u16))
}

// findImageInCfg 在 config.cfg 的镜像列表里按分区名查找对应镜像路径。
// partName 允许带 "boot:bootable" 这种 parameter.txt 风格的后缀，会自动截断到冒号前比较。
// 先按 config.cfg 里写的路径（相对 extractDir）解析；解析不到时按文件名在整个
// 压缩包范围内再找一次，兼容路径分隔符/大小写差异。
func findImageInCfg(items []CfgImageItem, files []string, extractDir, partName string) (string, bool) {
	name := partName
	if idx := strings.Index(name, ":"); idx >= 0 {
		name = name[:idx]
	}

	for _, it := range items {
		if !strings.EqualFold(it.Name, name) {
			continue
		}
		if !it.Selected {
			log.Printf("findImageInCfg: '%s' 在 config.cfg 中找到但未勾选烧录，跳过", partName)
			return "", false
		}
		if it.Path == "" {
			return "", false
		}

		p := it.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(extractDir, p)
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, true
		}

		base := filepath.Base(it.Path)
		for _, f := range files {
			if strings.EqualFold(filepath.Base(f), base) {
				return f, true
			}
		}
		log.Printf("findImageInCfg: '%s' -> '%s' 但文件不存在", partName, it.Path)
		return "", false
	}
	return "", false
}

// findImageByGuess 在没有 config.cfg（或 cfg 里查不到）时，按常见命名规则猜测镜像文件：
// <name>.img / <name>.raw / <name> / _<name>.img，大小写不敏感，在整个压缩包范围内查找。
func findImageByGuess(files []string, partName string) (string, bool) {
	name := partName
	if idx := strings.Index(name, ":"); idx >= 0 {
		name = name[:idx]
	}
	candidateNames := []string{name, name + ".img", name + ".raw", "_" + name + ".img"}

	for _, f := range files {
		base := filepath.Base(f)
		for _, c := range candidateNames {
			if strings.EqualFold(base, c) {
				return f, true
			}
		}
	}
	return "", false
}

// ============ SSE Progress ============

func handleProgress(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("id")
	if taskID == "" {
		jsonErr(w, http.StatusBadRequest, "缺少 task id")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	for {
		task := getTask(taskID)
		if task == nil {
			fmt.Fprintf(w, "data: {\"status\":\"error\",\"error\":\"task not found\"}\n\n")
			flusher.Flush()
			return
		}
		data, _ := json.Marshal(task)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
		if task.Status == "done" || task.Status == "error" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ============ Device Filter ============

func getFilteredDevices() []BlockDevice {
	cmd := exec.Command("lsblk", "-bdnJ", "-o", "NAME,SIZE,TYPE,MODEL,TRAN")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("lsblk command failed: %v", err)
		return getMockDevices()
	}

	var data LsblkOutput
	if err := json.Unmarshal(output, &data); err != nil {
		log.Printf("json unmarshal failed: %v, raw output: %s", err, string(output))
		return getMockDevices()
	}

	var result []BlockDevice
	for _, d := range data.BlockDevices {
		// 过滤掉 loop, ram, dm-, sr, zram 等虚拟/特殊设备
		if strings.HasPrefix(d.Name, "loop") || strings.HasPrefix(d.Name, "ram") ||
			strings.HasPrefix(d.Name, "dm-") || strings.HasPrefix(d.Name, "sr") ||
			strings.HasPrefix(d.Name, "zram") {
			continue
		}

		// 只保留 sd 和 nvme 设备
		if !strings.HasPrefix(d.Name, "sd") && !strings.HasPrefix(d.Name, "nvme") {
			continue
		}

		// 安全获取 tran 值（可能是 nil）
		tran := ""
		if d.Tran != nil {
			tran = *d.Tran
		}

		dt := "Disk"
		if strings.HasPrefix(d.Name, "nvme") {
			dt = "NVMe"
		} else if tran == "usb" {
			dt = "USB"
		} else if tran == "sata" || tran == "scsi" {
			dt = "SATA/SCSI"
		}

		// 【关键修复】json.Number 兼容数字和字符串，转成 int64
		sizeBytes, _ := d.Size.Int64()

		// 安全获取 model 值（可能是 nil）
		model := ""
		if d.Model != nil {
			model = *d.Model
		}

		result = append(result, BlockDevice{
			Name:  d.Name,
			Path:  "/dev/" + d.Name,
			Type:  dt,
			Size:  formatBytes(sizeBytes),
			Model: model,
		})
	}

	if len(result) == 0 {
		log.Println("No valid devices found, returning mock data")
		return getMockDevices()
	}

	return result
}

func getMockDevices() []BlockDevice {
	return []BlockDevice{
	}
}

func formatBytes(b int64) string {
	const unit = 1024
	if b <= 0 {
		return "0 B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}