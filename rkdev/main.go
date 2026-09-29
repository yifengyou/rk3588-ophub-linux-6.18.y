package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
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
	"github.com/nwaples/rardecode/v2"
	"golang.org/x/net/websocket"
)

//go:embed index.html
var indexHTML string

//go:embed res
var resFS embed.FS

// uploadDir 是所有本次上传相关临时文件（压缩包、单文件固件）的根目录
var uploadDir = "/tmp/kdev"

// unpackDir 是压缩包解压后的目标目录，位于 uploadDir 之下
var unpackDir = filepath.Join(uploadDir, "unpack")

const blockSize = 4 * 1024 * 1024

// paramSectorSize 是 parameter.txt 中 mtdparts 分区表 offset/size 的单位（扇区大小，字节）。
// Rockchip parameter.txt 里的数值是以 512 字节扇区为单位的，写入目标设备前需要换算成字节偏移量。
const paramSectorSize = 512

// gptReservedSectors 是磁盘头部预留给保护性 MBR + GPT 主分区表头 + 分区表数组的最小扇区数
// （1 个保护性 MBR + 1 个 GPT 头 + 128 个 128 字节分区表项 = 32 扇区，共 34 扇区）。
// parameter.txt 中起始扇区落在此区域内的分区（通常是 loader/idblock 等极早期区域）不通过
// GPT 管理，仍按原始偏移直接写入。
const gptReservedSectors = 34

// gptBackupSectors 是磁盘尾部预留给备份 GPT（备份分区表数组 32 扇区 + 备份头 1 扇区）的扇区数。
const gptBackupSectors = 33

// ============ Logging (terminal + /tmp/kdev.log) ============

const (
	logFilePath = "/tmp/kdev.log"
	logMaxSize  = 10 * 1024 * 1024 // 日志文件上限 10MB
)

// limitedFileWriter 以追加方式写入日志文件，并保证文件大小不超过 max。
// 当再写入一条日志会超过上限时，先清空文件再继续写入（相当于循环覆盖）。
// Write 永远不返回错误，避免文件写入失败时连带阻断 io.MultiWriter 中的终端输出。
type limitedFileWriter struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func newLimitedFileWriter(path string, max int64) (*limitedFileWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	w := &limitedFileWriter{path: path, max: max, f: f, size: st.Size()}
	// 上次遗留的文件如果已超限，启动时先清空
	if w.size > w.max {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
		w.size = 0
	}
	return w, nil
}

func (w *limitedFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	total := len(p)
	if int64(len(p)) > w.max {
		p = p[len(p)-int(w.max):] // 单条日志本身超限时只保留尾部
	}

	if w.size+int64(len(p)) > w.max {
		// O_APPEND 模式下 Truncate(0) 后，后续写入自动从 0 偏移开始
		if err := w.f.Truncate(0); err != nil {
			fmt.Fprintf(os.Stderr, "日志文件截断失败: %v\n", err)
			return total, nil
		}
		w.size = 0
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		fmt.Fprintf(os.Stderr, "日志文件写入失败: %v\n", err)
	}
	return total, nil
}

// initLogging 让所有 log 输出同时写到终端(stdout)和 /tmp/kdev.log
func initLogging() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	fw, err := newLimitedFileWriter(logFilePath, logMaxSize)
	if err != nil {
		log.SetOutput(os.Stdout)
		log.Printf("[LOG] 无法打开日志文件 %s，仅输出到终端: %v", logFilePath, err)
		return
	}
	log.SetOutput(io.MultiWriter(os.Stdout, fw))
	log.Printf("[LOG] 日志已启用：终端 + %s（追加写入，上限 %d MB）", logFilePath, logMaxSize/1024/1024)
}

// logRequests 记录 HTTP 请求（跳过高频的进度轮询和静态资源，避免刷屏）。
// 注意：不包装 ResponseWriter，因此不影响 SSE(Flusher) 与 WebSocket(Hijacker)。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p != "/api/progress" && p != "/api/upload_status" && !strings.HasPrefix(p, "/res/") {
			log.Printf("[HTTP] %s %s from %s", r.Method, p, r.RemoteAddr)
		}
		next.ServeHTTP(w, r)
	})
}

// ============ Upload Progress ============

// uploadState 记录当前这次上传的实时状态，供前端通过 /api/upload_status 轮询。
// Received 统计的是从网络实际收到的请求体字节数（含 multipart 头，略大于文件本身），
// Total 为请求 Content-Length，因此进度百分比是近似值。
// Stage: uploading=接收并写入磁盘；streaming=边接收边解压(tar.gz/tar.bz2)；extracting=接收完毕后解压(zip/rar)
type uploadState struct {
	Active   bool    `json:"active"`
	Stage    string  `json:"stage"`
	Name     string  `json:"name"`
	Total    int64   `json:"total"`
	Received int64   `json:"received"`
	Progress float64 `json:"progress"`
	Speed    float64 `json:"speed"` // 字节/秒
}

var (
	upMu    sync.Mutex
	upState uploadState
	upStart time.Time
)

func upBegin(total int64) {
	upMu.Lock()
	upState = uploadState{Active: true, Stage: "uploading", Total: total}
	upStart = time.Now()
	upMu.Unlock()
}

func upSet(name, stage string) {
	upMu.Lock()
	upState.Name = name
	upState.Stage = stage
	upMu.Unlock()
}

func upSetStage(stage string) {
	upMu.Lock()
	upState.Stage = stage
	upMu.Unlock()
}

func upEnd() {
	upMu.Lock()
	upState.Active = false
	st, el := upState, time.Since(upStart).Seconds()
	upMu.Unlock()
	if el > 0 {
		log.Printf("[UPLOAD] 传输结束: %s 共收到 %d bytes，用时 %.1fs，平均 %.2f MB/s",
			st.Name, st.Received, el, float64(st.Received)/el/1024/1024)
	}
}

// countingBody 包装请求体，统计实际收到的字节数（不改变读取行为）
type countingBody struct{ io.ReadCloser }

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if n > 0 {
		upMu.Lock()
		upState.Received += int64(n)
		if el := time.Since(upStart).Seconds(); el > 0 {
			upState.Speed = float64(upState.Received) / el
		}
		if upState.Total > 0 {
			upState.Progress = float64(upState.Received) / float64(upState.Total) * 100
			if upState.Progress > 100 {
				upState.Progress = 100
			}
		}
		upMu.Unlock()
	}
	return n, err
}

func handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	upMu.Lock()
	st := upState
	upMu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	jsonOK(w, st)
}

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

type FlashWriteItem struct {
	Name   string
	Offset int64
	Size   int64
	Path   string
}

type FlashTask struct {
	ID       string  `json:"id"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	Written  int64   `json:"written"`
	Total    int64   `json:"total"`
	Speed    float64 `json:"speed"`
	Error    string  `json:"error,omitempty"`
	Mode     string  `json:"mode,omitempty"`

	Items        []FlashWriteItem `json:"-"`
	Partitions   []ParamPartition `json:"-"` // 仅 Mode=="multi" 时有效，用于刷写前重建 GPT 分区表
	TargetDev    string           `json:"-"`
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

type LsblkDevice struct {
	Name  string      `json:"name"`
	Size  json.Number `json:"size"`
	Type  string      `json:"type"`
	Model *string     `json:"model"`
	Tran  *string     `json:"tran"`
}

// ============ Main ============

func main() {
	initLogging()

	tmpl := template.Must(template.New("index").Parse(indexHTML))

	resSubFS, err := fs.Sub(resFS, "res")
	if err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { tmpl.Execute(w, nil) })
	http.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, getFilteredDevices())
	})
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/api/progress", handleProgress)
	http.HandleFunc("/api/upload_status", handleUploadStatus)
	http.HandleFunc("/api/reboot", handleReboot)

	http.Handle("/api/terminal", websocket.Handler(handleTerminal))
	http.Handle("/res/", http.StripPrefix("/res/", http.FileServer(http.FS(resSubFS))))

	// 启动横幅使用 log，这样也会写入日志文件
	log.Println("========================================")
	log.Println("  RKdev + Terminal port:80 protocol:http")
	log.Println("========================================")
	log.Fatal(http.ListenAndServe(":80", logRequests(http.DefaultServeMux)))
}

// ============ Terminal (PTY over WebSocket) ============

func handleTerminal(ws *websocket.Conn) {
	defer ws.Close()
	log.Printf("[TERM] Session started from %s", ws.Request().RemoteAddr)

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

// checkFirmwareName 校验单一固件文件名（不含打包格式）
func checkFirmwareName(name string) (string, error) {
	lower := strings.ToLower(name)
	inner := lower
	if strings.HasSuffix(inner, ".gz") {
		inner = inner[:len(inner)-len(".gz")]
	}
	if strings.HasSuffix(inner, ".img") || strings.HasSuffix(inner, ".bin") {
		return name[:len(inner)], nil
	}
	return "", fmt.Errorf("不支持的文件类型 %q，仅支持 .img/.bin 固件及其 gzip 压缩包", name)
}

// ============ Archive Kind Detection ============

// archiveKind 标识上传文件所使用的打包格式
type archiveKind int

const (
	archiveNone archiveKind = iota
	archiveZip
	archiveTarGz
	archiveTarBz2
	archiveRar
)

func (k archiveKind) String() string {
	switch k {
	case archiveZip:
		return "zip"
	case archiveTarGz:
		return "tar.gz"
	case archiveTarBz2:
		return "tar.bz2"
	case archiveRar:
		return "rar"
	default:
		return "none"
	}
}

// detectArchiveKind 根据文件名判断打包格式：支持 zip / tar.gz(.tgz) / tar.bz2(.tbz2/.tbz) / rar
func detectArchiveKind(lowerName string) archiveKind {
	switch {
	case strings.HasSuffix(lowerName, ".zip"):
		return archiveZip
	case strings.HasSuffix(lowerName, ".tar.gz"), strings.HasSuffix(lowerName, ".tgz"):
		return archiveTarGz
	case strings.HasSuffix(lowerName, ".tar.bz2"), strings.HasSuffix(lowerName, ".tbz2"), strings.HasSuffix(lowerName, ".tbz"):
		return archiveTarBz2
	case strings.HasSuffix(lowerName, ".rar"):
		return archiveRar
	}
	return archiveNone
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	log.Printf("[UPLOAD] 收到上传请求 from %s", r.RemoteAddr)
	upBegin(r.ContentLength)
	defer upEnd()
	r.Body = &countingBody{ReadCloser: r.Body}

	// 每次上传前先清空 uploadDir，保证不会残留上一次上传的文件/解压产物，
	// 然后重新创建 uploadDir 及其下的 unpack 子目录。
	if err := os.RemoveAll(uploadDir); err != nil {
		jsonErr(w, http.StatusInternalServerError, "清理上传目录失败: "+err.Error())
		return
	}
	if err := os.MkdirAll(unpackDir, 0755); err != nil {
		jsonErr(w, http.StatusInternalServerError, "创建上传目录失败: "+err.Error())
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "解析表单失败: "+err.Error())
		return
	}

	var target, safeName, innerName, tmpPath string
	var written int64
	var isGzip bool
	var kind archiveKind
	var archivePath string // zip/rar 需要先落盘的路径

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
			kind = detectArchiveKind(lowerName)
			stage := "uploading"
			if kind == archiveTarGz || kind == archiveTarBz2 {
				stage = "streaming"
			}
			upSet(safeName, stage)

			// ==== 情况1: tar.gz / tgz — 流式解压，不落盘压缩包 ====
			if kind == archiveTarGz {
				extractDir := filepath.Join(unpackDir, fmt.Sprintf("pkg_%d_targz", time.Now().UnixNano()))
				if err := os.MkdirAll(extractDir, 0755); err != nil {
					jsonErr(w, http.StatusInternalServerError, "创建解压目录失败: "+err.Error())
					return
				}
				files, err := extractTarGz(part, extractDir)
				if err != nil {
					os.RemoveAll(extractDir)
					jsonErr(w, http.StatusBadRequest, "解压 tar.gz 失败: "+err.Error())
					return
				}
				if len(files) == 0 {
					os.RemoveAll(extractDir)
					jsonErr(w, http.StatusBadRequest, "tar.gz 包内没有常规文件")
					return
				}
				task, msg, err := buildTaskFromExtracted(files, extractDir, target)
				if err != nil {
					os.RemoveAll(extractDir)
					jsonErr(w, http.StatusBadRequest, err.Error())
					return
				}
				log.Printf("[UPLOAD] %s -> tar.gz 解压到 %s，%d 文件，mode=%s -> %s",
					safeName, extractDir, len(files), task.Mode, target)
				go doFlash(task)
				jsonOK(w, map[string]string{"task_id": task.ID, "message": msg})
				return
			}

			// ==== 情况2: tar.bz2 / tbz2 / tbz — 流式解压，不落盘压缩包 ====
			if kind == archiveTarBz2 {
				extractDir := filepath.Join(unpackDir, fmt.Sprintf("pkg_%d_tarbz2", time.Now().UnixNano()))
				if err := os.MkdirAll(extractDir, 0755); err != nil {
					jsonErr(w, http.StatusInternalServerError, "创建解压目录失败: "+err.Error())
					return
				}
				files, err := extractTarBz2(part, extractDir)
				if err != nil {
					os.RemoveAll(extractDir)
					jsonErr(w, http.StatusBadRequest, "解压 tar.bz2 失败: "+err.Error())
					return
				}
				if len(files) == 0 {
					os.RemoveAll(extractDir)
					jsonErr(w, http.StatusBadRequest, "tar.bz2 包内没有常规文件")
					return
				}
				task, msg, err := buildTaskFromExtracted(files, extractDir, target)
				if err != nil {
					os.RemoveAll(extractDir)
					jsonErr(w, http.StatusBadRequest, err.Error())
					return
				}
				log.Printf("[UPLOAD] %s -> tar.bz2 解压到 %s，%d 文件，mode=%s -> %s",
					safeName, extractDir, len(files), task.Mode, target)
				go doFlash(task)
				jsonOK(w, map[string]string{"task_id": task.ID, "message": msg})
				return
			}

			// ==== 情况3: zip / rar — 需先整体落盘再解压 ====
			if kind == archiveZip || kind == archiveRar {
				ext := ".zip"
				if kind == archiveRar {
					ext = ".rar"
				}
				archivePath = filepath.Join(uploadDir, fmt.Sprintf("pkg_%d%s", time.Now().UnixNano(), ext))
				dst, cerr := os.Create(archivePath)
				if cerr != nil {
					jsonErr(w, http.StatusInternalServerError, "创建临时文件失败: "+cerr.Error())
					return
				}
				written, err = io.Copy(dst, part)
				dst.Close()
				if err != nil {
					os.Remove(archivePath)
					jsonErr(w, http.StatusInternalServerError, "保存固件包失败: "+err.Error())
					return
				}
				if written == 0 {
					os.Remove(archivePath)
					jsonErr(w, http.StatusBadRequest, "文件内容为空")
					return
				}
			} else {
				// ==== 情况4: 单文件 .img/.bin 或 .gz ====
				innerName, err = checkFirmwareName(safeName)
				if err != nil {
					jsonErr(w, http.StatusBadRequest, err.Error())
					return
				}
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

	// 后续处理（zip / rar / 单文件）；tar.gz / tar.bz2 已在上面提前 return
	if kind != archiveZip && kind != archiveRar && tmpPath == "" {
		jsonErr(w, http.StatusBadRequest, "获取文件失败: 缺少 firmware 文件")
		return
	}
	if target == "" || !strings.HasPrefix(target, "/dev/") || strings.Contains(target, "..") {
		if kind == archiveZip || kind == archiveRar {
			os.Remove(archivePath)
		} else {
			os.Remove(tmpPath)
		}
		jsonErr(w, http.StatusBadRequest, "非法目标设备路径")
		return
	}

	var task *FlashTask
	var msg string

	if kind == archiveZip || kind == archiveRar {
		upSetStage("extracting")
		extractDir := filepath.Join(unpackDir, fmt.Sprintf("pkg_%d", time.Now().UnixNano()))
		if err := os.MkdirAll(extractDir, 0755); err != nil {
			os.Remove(archivePath)
			jsonErr(w, http.StatusInternalServerError, "创建解压目录失败: "+err.Error())
			return
		}

		var files []string
		if kind == archiveZip {
			files, err = extractZip(archivePath, extractDir)
		} else {
			files, err = extractRar(archivePath, extractDir)
		}
		// 压缩包解压后保留，下一次上传时随 uploadDir 一并清空
		if err != nil {
			os.RemoveAll(extractDir)
			jsonErr(w, http.StatusBadRequest, fmt.Sprintf("解压 %s 固件包失败: %v", kind, err))
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

		msg = fmt.Sprintf("文件 %s (%d MB) 已暂存，正在刷写到 %s ...", safeName, written/1024/1024, target)
		if isGzip {
			msg = fmt.Sprintf("gzip 固件 %s 已自动解压为 %s (%d MB)，正在刷写到 %s ...",
				safeName, innerName, written/1024/1024, target)
		}
		log.Printf("[UPLOAD] %s -> %s (%d bytes, gzip=%v) -> %s", safeName, tmpPath, written, isGzip, target)
	}

	go doFlash(task)

	jsonOK(w, map[string]string{
		"task_id": task.ID,
		"message": msg,
	})
}

// ============ Archive Extractors ============

// extractTarGz 流式解压 tar.gz 到 destDir，返回解出的常规文件路径列表
func extractTarGz(reader io.Reader, destDir string) ([]string, error) {
	gzReader, err := gzip.NewReader(reader)
	if err != nil {
		return nil, fmt.Errorf("gzip 解压初始化失败: %w", err)
	}
	defer gzReader.Close()

	return extractTarStream(gzReader, destDir)
}

// extractTarBz2 流式解压 tar.bz2 到 destDir，返回解出的常规文件路径列表
func extractTarBz2(reader io.Reader, destDir string) ([]string, error) {
	bz2Reader := bzip2.NewReader(reader)
	return extractTarStream(bz2Reader, destDir)
}

// extractTarStream 是 tar 层的通用解压逻辑，供 gzip / bzip2 等上层解压器复用
func extractTarStream(tarStream io.Reader, destDir string) ([]string, error) {
	tarReader := tar.NewReader(tarStream)
	var files []string

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 tar 条目失败: %w", err)
		}

		cleanName := filepath.Clean(header.Name)
		if cleanName == ".." || strings.HasPrefix(cleanName, "../") || filepath.IsAbs(cleanName) {
			log.Printf("extractTarStream: 跳过可疑路径: %s", header.Name)
			continue
		}
		cleanName = filepath.FromSlash(cleanName)
		outPath := filepath.Join(destDir, cleanName)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(outPath, os.FileMode(header.Mode)); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
				return nil, err
			}
			mode := os.FileMode(header.Mode)
			if mode == 0 {
				mode = 0644
			}
			outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return nil, fmt.Errorf("写入 %s 失败: %w", outPath, err)
			}
			outFile.Close()
			files = append(files, outPath)
		}
	}
	return files, nil
}

// extractRar 解压 RAR 文件到 destDir，返回解出的常规文件路径列表
func extractRar(rarPath, destDir string) ([]string, error) {
	r, err := rardecode.OpenReader(rarPath)
	if err != nil {
		return nil, fmt.Errorf("打开 RAR 文件失败: %w", err)
	}
	defer r.Close()

	var files []string
	for {
		header, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("读取 RAR 条目失败: %w", err)
		}

		// 防御路径穿越
		cleanName := filepath.Clean(header.Name)
		if cleanName == ".." || strings.HasPrefix(cleanName, "../") || filepath.IsAbs(cleanName) {
			log.Printf("extractRar: 跳过可疑路径: %s", header.Name)
			continue
		}
		cleanName = filepath.FromSlash(cleanName)
		outPath := filepath.Join(destDir, cleanName)

		if header.IsDir {
			if err := os.MkdirAll(outPath, 0755); err != nil {
				return nil, err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
			return nil, err
		}

		outFile, err := os.Create(outPath)
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(outFile, r); err != nil {
			outFile.Close()
			return nil, fmt.Errorf("写入 %s 失败: %w", outPath, err)
		}
		outFile.Close()
		files = append(files, outPath)
	}
	return files, nil
}

// extractZip 解压 ZIP 文件到 destDir，返回解出的常规文件路径列表
func extractZip(zipPath, destDir string) ([]string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	var files []string
	for _, f := range r.File {
		cleanName := filepath.Clean(f.Name)
		if cleanName == ".." || strings.HasPrefix(cleanName, "../") || filepath.IsAbs(cleanName) {
			log.Printf("extractZip: 跳过可疑路径: %s", f.Name)
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

// ============ Task Building From Extracted Files ============

// buildTaskFromExtracted 根据解压后的文件列表构建刷写任务。
//
//   - 只有一个文件：直接整盘写入目标设备。
//   - 多个文件：交由 buildMultiFileTask 处理（package-file/config.cfg + parameter.txt）。
func buildTaskFromExtracted(files []string, extractDir, target string) (*FlashTask, string, error) {
	if len(files) == 0 {
		return nil, "", fmt.Errorf("固件包内没有解压出任何文件")
	}

	if len(files) == 1 {
		return buildSingleFileTask(files[0], extractDir, target)
	}

	return buildMultiFileTask(files, extractDir, target)
}

// buildSingleFileTask 解压结果只有一个文件时，直接将其整盘写入目标设备
func buildSingleFileTask(filePath, extractDir, target string) (*FlashTask, string, error) {
	st, err := os.Stat(filePath)
	if err != nil {
		return nil, "", fmt.Errorf("读取解压文件失败: %v", err)
	}

	task := newTask(target, st.Size())
	task.Mode = "single"
	task.Items = []FlashWriteItem{{
		Name: filepath.Base(filePath), Offset: 0,
		Size: st.Size(), Path: filePath,
	}}

	msg := fmt.Sprintf("固件包内只有单一文件 %s (%d MB)，将整盘写入 %s ...",
		filepath.Base(filePath), st.Size()/1024/1024, target)
	return task, msg, nil
}

// buildMultiFileTask 处理解压结果包含多个文件时的刷写逻辑。
//
// 支持两种多文件固件包格式，均需要 parameter.txt 分区表（mtdparts，offset/size 单位为 512
// 字节扇区）配合使用，按“分区名 -> 镜像文件路径”的映射逐一写入对应偏移：
//
//  1. package-file + parameter.txt（Rockchip update.img / afptool 风格）：
//     package-file 是一个文本文件，每行 "<名称> <相对路径>"，将分区/组件名映射到固件包内的
//     相对文件路径，例如 "boot Image/boot.img"。
//
//  2. config.cfg + parameter.txt（RKDevTool 风格）：
//     config.cfg 是 RKDevTool 使用的二进制配置文件（"CFG" 魔数开头），记录镜像名称、路径及是否
//     勾选写入，通过 parseConfigCfg / findImageInCfg 解析。
//
// 若固件包内既没有 package-file 也没有 config.cfg，则退化为按分区名直接猜测文件名
// （findImageByGuess，如分区名 "boot" 对应文件 "boot.img"）。
//
// parameter.txt 中未能匹配到任何镜像文件的分区会被跳过（这是正常情况，因为分区表里常常包含
// 一些仅用于占位、不随固件包分发独立镜像的分区）；只有当所有分区都匹配失败时才报错。
func buildMultiFileTask(files []string, extractDir, target string) (*FlashTask, string, error) {
	// 1. 定位并解析 parameter.txt 分区表
	paramPath := findByBaseNames(files, []string{"parameter.txt", "parameter"})
	if paramPath == "" {
		return nil, "", fmt.Errorf("固件包内包含 %d 个文件，但未找到 parameter.txt 分区表，无法确定多文件写入方式", len(files))
	}
	partitions, err := parseParameterFile(paramPath)
	if err != nil {
		return nil, "", fmt.Errorf("解析 parameter.txt 失败: %v", err)
	}

	// 2. 定位镜像描述文件：优先 package-file，其次 config.cfg，都没有则按文件名猜测
	var pkgItems []PackageFileItem
	var cfgItems []CfgImageItem
	var sourceDesc string

	if pkgPath := findByBaseNames(files, []string{"package-file"}); pkgPath != "" {
		pkgItems, err = parsePackageFile(pkgPath)
		if err != nil {
			return nil, "", fmt.Errorf("解析 package-file 失败: %v", err)
		}
		sourceDesc = "package-file"
	} else if cfgPath := findConfigCfg(files); cfgPath != "" {
		cfgItems, err = parseConfigCfg(cfgPath)
		if err != nil {
			return nil, "", fmt.Errorf("解析 config.cfg 失败: %v", err)
		}
		sourceDesc = "config.cfg"
	} else {
		sourceDesc = "文件名猜测"
	}

	// 3. 按分区表逐一匹配镜像文件，构建写入任务
	var items []FlashWriteItem
	var totalSize int64
	var skipped []string

	for _, part := range partitions {
		if part.Name == "" {
			continue
		}

		var imgPath string
		var found bool

		switch {
		case pkgItems != nil:
			imgPath, found = findImageInPackage(pkgItems, files, extractDir, part.Name)
		case cfgItems != nil:
			imgPath, found = findImageInCfg(cfgItems, files, extractDir, part.Name)
		default:
			imgPath, found = findImageByGuess(files, part.Name)
		}

		if !found {
			skipped = append(skipped, part.Name)
			continue
		}

		st, serr := os.Stat(imgPath)
		if serr != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			skipped = append(skipped, part.Name)
			if serr != nil {
				log.Printf("buildMultiFileTask: stat '%s' 失败: %v", imgPath, serr)
			}
			continue
		}

		offsetBytes := int64(part.Offset) * paramSectorSize
		items = append(items, FlashWriteItem{
			Name:   part.Name,
			Offset: offsetBytes,
			Size:   st.Size(),
			Path:   imgPath,
		})
		totalSize += st.Size()
	}

	if len(items) == 0 {
		return nil, "", fmt.Errorf("根据 parameter.txt 的 %d 个分区，未能匹配到任何可写入的镜像文件（描述文件来源: %s）", len(partitions), sourceDesc)
	}

	task := newTask(target, totalSize)
	task.Mode = "multi"
	task.Items = items
	// 保存完整分区表（而非仅匹配到镜像的部分），用于刷写前按磁盘实际容量重建 GPT 分区表，
	// 这样即使某些分区本次没有对应镜像文件，也能在分区表中正确保留位置。
	task.Partitions = partitions

	msg := fmt.Sprintf("固件包解析完成（%s + parameter.txt），%d 个分区匹配到镜像（%d 个分区未匹配，已跳过），共 %d MB，将重建 GPT 分区表后写入 %s ...",
		sourceDesc, len(items), len(skipped), totalSize/1024/1024, target)
	if len(skipped) > 0 {
		log.Printf("[MULTI] 以下分区未找到对应镜像文件，已跳过: %v", skipped)
	}
	for _, it := range items {
		log.Printf("[MULTI] 分区 '%s' -> %s (%d bytes) @ offset 0x%X", it.Name, it.Path, it.Size, it.Offset)
	}

	return task, msg, nil
}

// findByBaseName 在文件列表中按文件名查找（大小写不敏感）
func findByBaseName(files []string, name string) string {
	for _, f := range files {
		if strings.EqualFold(filepath.Base(f), name) {
			return f
		}
	}
	return ""
}

// findByBaseNames 在文件列表中按一组候选文件名依次查找（大小写不敏感），返回第一个命中的路径
func findByBaseNames(files []string, names []string) string {
	for _, n := range names {
		if p := findByBaseName(files, n); p != "" {
			return p
		}
	}
	return ""
}

// findConfigCfg 在文件列表中查找 RKDevTool 风格的 config.cfg。
// 优先按常见文件名 "config.cfg" 查找并用魔数校验；若未命中，再遍历全部文件用魔数 "CFG" 识别，
// 因为不同厂商打包时该文件名可能不同。
func findConfigCfg(files []string) string {
	if p := findByBaseName(files, "config.cfg"); p != "" && isConfigCfg(p) {
		return p
	}
	for _, f := range files {
		if isConfigCfg(f) {
			return f
		}
	}
	return ""
}

// doFlash 执行刷写
func doFlash(task *FlashTask) {
	// 刷机结束后不删除上传/解压文件，仅在下一次上传开始时统一清空 uploadDir
	// 统一记录失败日志（错误原本只写入 task 状态，日志里看不到）
	defer func() {
		if t := getTask(task.ID); t != nil && t.Status == "error" {
			log.Printf("[%s] ❌ Flash failed: %s", task.ID, t.Error)
		}
	}()

	log.Printf("[%s] Flash start: mode=%s, %d item(s) -> %s (%d bytes)",
		task.ID, task.Mode, len(task.Items), task.TargetDev, task.Total)

	// 多文件刷写前先按 parameter.txt 重建 GPT 分区表，避免设备上残留与当前磁盘容量不匹配、
	// 备份表损坏等陈旧分区信息（即 "GPT PMBR size mismatch" / "backup GPT table is corrupt" 问题）。
	if task.Mode == "multi" && len(task.Partitions) > 0 {
		updateTask(task.ID, func(t *FlashTask) { t.Status = "partitioning" })
		if err := applyGPTPartitionTable(task.TargetDev, task.Partitions); err != nil {
			updateTask(task.ID, func(t *FlashTask) {
				t.Status = "error"
				t.Error = "重建 GPT 分区表失败: " + err.Error()
			})
			return
		}
		updateTask(task.ID, func(t *FlashTask) { t.Status = "writing" })
	}

	dst, err := os.OpenFile(task.TargetDev, os.O_WRONLY|os.O_SYNC, 0)
	if err != nil {
		updateTask(task.ID, func(t *FlashTask) {
			t.Status = "error"
			t.Error = "打开目标设备失败: " + err.Error()
		})
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
				t.Error = fmt.Sprintf("定位分区 '%s' 失败: %v", item.Name, err)
			})
			return
		}

		log.Printf("[%s] Writing '%s' (%d bytes) @ offset %d", task.ID, item.Name, item.Size, item.Offset)

		for {
			n, readErr := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					src.Close()
					updateTask(task.ID, func(t *FlashTask) {
						t.Status = "error"
						t.Error = fmt.Sprintf("写入 '%s' 失败 @ %d bytes: %v", item.Name, totalWritten, werr)
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
	log.Printf("[%s] ✅ Done: %d bytes, %.1fs, %.1f MB/s",
		task.ID, totalWritten, elapsed, float64(totalWritten)/elapsed/1024/1024)
}

// ============ GPT Partition Table Rebuild (sgdisk) ============

// blockDeviceSectors 读取目标块设备的实际容量（单位：512 字节扇区），通过
// /sys/class/block/<dev>/size 获取，避免依赖外部工具解析。
func blockDeviceSectors(target string) (uint64, error) {
	name := strings.TrimPrefix(target, "/dev/")
	data, err := os.ReadFile(filepath.Join("/sys/class/block", name, "size"))
	if err != nil {
		return 0, fmt.Errorf("读取 %s 容量失败: %w", target, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("解析 %s 容量失败: %w", target, err)
	}
	return v, nil
}

// applyGPTPartitionTable 依据 parameter.txt 中的分区表，在目标设备上按磁盘当前实际容量
// 重新创建一份 GPT 分区表（通过 sgdisk 完成）。
//
// 这是多文件刷写（package-file/config.cfg + parameter.txt）流程的必要前置步骤：如果不重建
// 分区表，直接按偏移量写入镜像会导致设备上残留旧的、与当前磁盘容量不匹配的 GPT
// （典型症状如 fdisk 报 "GPT PMBR size mismatch"、"backup GPT table is corrupt"、
// "backup GPT table is not on the end of the device"），系统重启后可能无法正确识别分区。
//
// 流程：
//  1. sgdisk --zap-all：销毁设备上现有的 GPT / MBR 数据结构（即使已损坏也能处理）；
//  2. sgdisk -o -a 1 -n ... -c ... -u ...：新建空 GPT（-o），关闭对齐（-a 1，保证分区起始
//     扇区与 parameter.txt 完全一致），并逐个创建分区、设置名称及（若 parameter.txt 中通过
//     "uuid:<分区名>=<UUID>" 指定）强制的 PARTUUID。
//
// 分区表项的起止扇区直接取自 parameter.txt（单位为 512 字节扇区）；size 为 0xFFFFFFFF
// （parameter.txt 中以 "-" 表示，通常是最后一个分区，如 rootfs/userdata）的分区，结束扇区
// 传 0，由 sgdisk 自动延伸到最后一个可用扇区（并为备份 GPT 预留空间）。
// 起始扇区落在 GPT 保留区（前 gptReservedSectors 个扇区）内的分区不纳入 GPT 管理，仍按原始
// 偏移直接写入（doFlash 中的逐项写入逻辑本身不受影响）。
func applyGPTPartitionTable(target string, partitions []ParamPartition) error {
	sgdiskPath, err := exec.LookPath("sgdisk")
	if err != nil {
		return fmt.Errorf("未找到 sgdisk 工具，无法重建 GPT 分区表（请安装 gdisk / gptfdisk 软件包）: %w", err)
	}

	diskSectors, err := blockDeviceSectors(target)
	if err != nil {
		return err
	}
	if diskSectors == 0 {
		return fmt.Errorf("无法获取 %s 的磁盘容量", target)
	}

	args, created := buildSgdiskArgs(partitions, diskSectors)
	if len(created) == 0 {
		return fmt.Errorf("parameter.txt 中没有可用于创建 GPT 分区的有效分区项")
	}

	// 第一步：清除现有的 GPT / MBR。对已损坏的分区表 sgdisk 可能给出警告甚至非零退出码，
	// 此处仅记录日志，不视为致命错误——后续带 -o 的命令会再次完整重建分区表。
	zapOut, zapErr := exec.Command(sgdiskPath, "--zap-all", target).CombinedOutput()
	if zapErr != nil {
		log.Printf("[GPT] sgdisk --zap-all %s 返回错误（忽略，继续重建）: %v, output: %s", target, zapErr, string(zapOut))
	}

	// 第二步：新建 GPT 并创建全部分区
	fullArgs := append([]string{"-o", "-a", "1"}, args...)
	fullArgs = append(fullArgs, target)
	out, err := exec.Command(sgdiskPath, fullArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sgdisk 写入分区表失败: %v, output: %s\n命令: sgdisk %s", err, string(out), strings.Join(fullArgs, " "))
	}

	rereadPartitionTable(target)

	log.Printf("[GPT] 已通过 sgdisk 在 %s 上重建 GPT 分区表，共 %d 个分区: %v，磁盘容量 %d 扇区\n命令: sgdisk %s\nsgdisk 输出:\n%s",
		target, len(created), created, diskSectors, strings.Join(fullArgs, " "), string(out))
	return nil
}

// buildSgdiskArgs 依据 parameter.txt 分区表及磁盘实际容量，生成 sgdisk 的分区相关参数
// （-n / -c / -u），并返回实际创建的分区名列表。分区编号按创建顺序从 1 开始连续编号。
//
// 每个分区生成形如：
//
//	-n 1:64:6207            起始扇区:结束扇区（结束扇区为 0 表示延伸到最后一个可用扇区）
//	-c 1:uboot              分区名称
//	-u 1:614e0000-...       （可选）强制指定分区 GUID，即内核命令行中的 PARTUUID
func buildSgdiskArgs(partitions []ParamPartition, diskSectors uint64) ([]string, []string) {
	var args []string
	var created []string

	// GPT 最后一个可用扇区（备份 GPT 位于磁盘末尾 gptBackupSectors 个扇区内）
	var lastUsable uint64
	if diskSectors > gptBackupSectors+1 {
		lastUsable = diskSectors - gptBackupSectors - 1
	}

	for _, p := range partitions {
		name := p.Name
		if idx := strings.Index(name, ":"); idx >= 0 {
			name = name[:idx]
		}
		if name == "" {
			continue
		}
		if uint64(p.Offset) < gptReservedSectors {
			log.Printf("[GPT] 分区 '%s' 起始扇区 %d 落在 GPT 保留区内（<%d），跳过 GPT 分区创建，仍按原始偏移写入",
				name, p.Offset, gptReservedSectors)
			continue
		}
		if uint64(p.Offset) > lastUsable {
			log.Printf("[GPT] 分区 '%s' 起始扇区 %d 超出磁盘可用范围（最后可用扇区 %d），跳过", name, p.Offset, lastUsable)
			continue
		}

		// 结束扇区：size 为 "-" 或超出磁盘可用范围时传 0，让 sgdisk 自动延伸到最后可用扇区
		var end uint64
		if p.Size != 0xFFFFFFFF && p.Size != 0 {
			e := uint64(p.Offset) + uint64(p.Size) - 1
			if e <= lastUsable {
				end = e
			}
		}

		num := len(created) + 1
		args = append(args,
			"-n", fmt.Sprintf("%d:%d:%d", num, p.Offset, end),
			"-c", fmt.Sprintf("%d:%s", num, name),
		)

		if p.UUID != "" {
			if isValidGUID(p.UUID) {
				args = append(args, "-u", fmt.Sprintf("%d:%s", num, p.UUID))
				log.Printf("[GPT] 分区 '%s' (#%d) 强制 PARTUUID = %s", name, num, p.UUID)
			} else {
				log.Printf("[GPT] 分区 '%s' 的 UUID %q 格式非法，忽略，将使用随机 PARTUUID", name, p.UUID)
			}
		}

		created = append(created, name)
	}

	return args, created
}

// isValidGUID 校验形如 xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx 的 GUID 字符串
func isValidGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// rereadPartitionTable 通知内核重新读取目标设备的分区表，优先使用 partprobe，
// 不可用时回退到 blockdev --rereadpt；都不可用时仅记录日志，不视为致命错误
// （分区表本身已经正确写入设备，内核视图会在下次插拔/重启后自动刷新）。
func rereadPartitionTable(target string) {
	if p, err := exec.LookPath("partprobe"); err == nil {
		if out, err := exec.Command(p, target).CombinedOutput(); err != nil {
			log.Printf("[GPT] partprobe %s 失败: %v, output: %s", target, err, string(out))
		}
		return
	}
	if p, err := exec.LookPath("blockdev"); err == nil {
		if out, err := exec.Command(p, "--rereadpt", target).CombinedOutput(); err != nil {
			log.Printf("[GPT] blockdev --rereadpt %s 失败: %v, output: %s", target, err, string(out))
		}
		return
	}
	log.Printf("[GPT] 未找到 partprobe/blockdev，内核分区表视图可能需要重新插拔设备或重启后才能刷新")
}

// ============ parameter.txt parsing ============

type ParamPartition struct {
	Name   string
	Offset uint32
	Size   uint32
	UUID   string // 来自 parameter.txt 中 "uuid:<分区名>=<UUID>" 行，为空表示不强制
}

func parseParameterFile(path string) ([]ParamPartition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseParameterBytes(string(data))
}

// partBaseName 返回分区名去掉 ":grow" 等冒号后缀后的部分
func partBaseName(name string) string {
	if idx := strings.Index(name, ":"); idx >= 0 {
		return name[:idx]
	}
	return name
}

// parseUUIDLine 解析形如 "uuid:rootfs=614e0000-0000-4b53-8000-1d28000054a9" 的行，
// 返回分区名与 UUID。不是 uuid 行时 ok 为 false。
func parseUUIDLine(line string) (name, uuid string, ok bool) {
	if len(line) < 5 || !strings.EqualFold(line[:5], "uuid:") {
		return "", "", false
	}
	rest := line[5:]
	eq := strings.Index(rest, "=")
	if eq < 0 {
		return "", "", false
	}
	name = strings.TrimSpace(rest[:eq])
	uuid = strings.TrimSpace(rest[eq+1:])
	if name == "" || uuid == "" {
		return "", "", false
	}
	return name, uuid, true
}

func parseParameterBytes(content string) ([]ParamPartition, error) {
	var result []ParamPartition
	uuidMap := make(map[string]string) // 分区名(小写) -> UUID

	content = strings.ReplaceAll(content, "\r\n", "\n")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// uuid:<分区名>=<UUID> 行：记录需要强制指定的 PARTUUID
		if n, u, ok := parseUUIDLine(line); ok {
			uuidMap[strings.ToLower(partBaseName(n))] = u
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

	// 将 uuid 映射应用到对应分区
	used := make(map[string]bool)
	for i := range result {
		key := strings.ToLower(partBaseName(result[i].Name))
		if u, ok := uuidMap[key]; ok {
			result[i].UUID = u
			used[key] = true
		}
	}
	for k, u := range uuidMap {
		if !used[k] {
			log.Printf("parseParameterBytes: uuid:%s=%s 没有对应的分区，已忽略", k, u)
		}
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

// ============ package-file parsing (Rockchip update.img / afptool 风格) ============

// PackageFileItem 表示 package-file 中的一条镜像条目：名称 -> 固件包内相对路径
type PackageFileItem struct {
	Name string
	Path string
}

// parsePackageFile 解析 Rockchip package-file。
//
// 典型格式（"#" 开头为注释，空行忽略，每行由任意空白分隔为“名称 路径”两列）：
//
//	# NAME			PATH
//	package-file		package-file
//	parameter		parameter.txt
//	bootloader		MiniLoaderAll.bin
//	boot			boot.img
//	rootfs			rootfs.img
//
// 名称对应 parameter.txt 分区表中的分区名，路径是相对于固件包解压目录的相对路径
// （也可能带有子目录，如 "Image/boot.img"）。
func parsePackageFile(path string) ([]PackageFileItem, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parsePackageFileBytes(string(data))
}

func parsePackageFileBytes(content string) ([]PackageFileItem, error) {
	var items []PackageFileItem
	content = strings.ReplaceAll(content, "\r\n", "\n")
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[0], "/")
		p := strings.ReplaceAll(fields[1], "\\", "/")
		items = append(items, PackageFileItem{Name: name, Path: p})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("package-file 内没有解析出任何有效条目")
	}
	return items, nil
}

// findImageInPackage 在 package-file 条目中按分区名查找对应镜像文件的实际路径。
// partName 可能带有形如 "boot:grow" 的后缀（增长标记等），仅取冒号前的部分用于匹配。
func findImageInPackage(items []PackageFileItem, files []string, extractDir, partName string) (string, bool) {
	name := partName
	if idx := strings.Index(name, ":"); idx >= 0 {
		name = name[:idx]
	}

	for _, it := range items {
		if !strings.EqualFold(it.Name, name) {
			continue
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

		// 路径本身可能因不同打包工具而与解压目录结构不完全一致，回退按文件名匹配
		base := filepath.Base(it.Path)
		for _, f := range files {
			if strings.EqualFold(filepath.Base(f), base) {
				return f, true
			}
		}
		log.Printf("findImageInPackage: '%s' -> '%s' 文件不存在", partName, it.Path)
		return "", false
	}
	return "", false
}

// ============ config.cfg parsing (RKDevTool 风格) ============

const (
	cfgHeaderSize  = 29
	cfgNameUnits   = 40
	cfgPathUnits   = 260
	cfgMinItemSize = 2 + cfgNameUnits*2 + cfgPathUnits*2 + 4 + 1
	cfgAddrAuto    = 0xFFFFFFFF
)

type CfgImageItem struct {
	Name     string
	Path     string
	Address  uint32
	Selected bool
}

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
		return nil, fmt.Errorf("itemSize=%d 过小（至少 %d）", itemSize, cfgMinItemSize)
	}
	if int(begin) < cfgHeaderSize || int(begin) > len(data) {
		return nil, fmt.Errorf("begin=0x%X 超出文件范围", begin)
	}

	payload := len(data) - int(begin)
	entries := payload / int(itemSize)
	if remainder := payload % int(itemSize); remainder != 0 {
		log.Printf("parseConfigCfg: 末尾多出 %d 字节", remainder)
	}

	items := make([]CfgImageItem, 0, entries)
	off := int(begin)
	for i := 0; i < entries; i++ {
		if off+int(itemSize) > len(data) {
			break
		}
		rec := data[off : off+int(itemSize)]
		off += int(itemSize)

		selfSize := binary.LittleEndian.Uint16(rec[0:2])
		if selfSize != itemSize {
			log.Printf("parseConfigCfg: item %d size(%d) != header.itemSize(%d)", i, selfSize, itemSize)
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
			log.Printf("findImageInCfg: '%s' 未勾选，跳过", partName)
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
		log.Printf("findImageInCfg: '%s' -> '%s' 文件不存在", partName, it.Path)
		return "", false
	}
	return "", false
}

func findImageByGuess(files []string, partName string) (string, bool) {
	name := partName
	if idx := strings.Index(name, ":"); idx >= 0 {
		name = name[:idx]
	}
	candidates := []string{name, name + ".img", name + ".raw", "_" + name + ".img"}

	for _, f := range files {
		base := filepath.Base(f)
		for _, c := range candidates {
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
		log.Printf("lsblk failed: %v", err)
		return getMockDevices()
	}

	var data LsblkOutput
	if err := json.Unmarshal(output, &data); err != nil {
		log.Printf("json unmarshal failed: %v", err)
		return getMockDevices()
	}

	var result []BlockDevice
	for _, d := range data.BlockDevices {
		if strings.HasPrefix(d.Name, "loop") || strings.HasPrefix(d.Name, "ram") ||
			strings.HasPrefix(d.Name, "dm-") || strings.HasPrefix(d.Name, "sr") ||
			strings.HasPrefix(d.Name, "zram") {
			continue
		}
		if !strings.HasPrefix(d.Name, "sd") && !strings.HasPrefix(d.Name, "nvme") {
			continue
		}

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

		sizeBytes, _ := d.Size.Int64()
		model := ""
		if d.Model != nil {
			model = *d.Model
		}

		result = append(result, BlockDevice{
			Name: d.Name, Path: "/dev/" + d.Name,
			Type: dt, Size: formatBytes(sizeBytes), Model: model,
		})
	}

	if len(result) == 0 {
		return getMockDevices()
	}
	return result
}

func getMockDevices() []BlockDevice {
	return []BlockDevice{}
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

