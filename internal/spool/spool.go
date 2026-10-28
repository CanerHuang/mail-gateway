// Package spool 管理非同步佇列的檔案：一封郵件一個檔案，佇列就是 queue/ 資料夾。
//
// 檔案格式：第一行是寄送資訊（Meta，JSON 一行），第二行之後是完整的 MIME 郵件
// （不含 Date header，寄出時才補上）。檔名為 <下次寄送時間>_<mail_id>[_<失敗次數>].mail，
// 掃描時只要列出資料夾，不必打開每個檔案。
package spool

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"maild/internal/config"
)

const fileExt = ".mail"

// ErrCorrupted 表示佇列檔無法解析（第一行過長、不是合法 JSON 等），只能刪除。
var ErrCorrupted = errors.New("corrupted queue file")

// Meta 為佇列檔第一行的寄送資訊。郵件內容在第二行之後，這裡只放寄送與 log 需要的小資料。
type Meta struct {
	MailID      string            `json:"mail_id"`
	Client      string            `json:"client"`
	Sender      string            `json:"sender"` // 寄件者別名
	From        string            `json:"from"`   // 收下時的寄件地址，寄出前用來比對設定是否已變更
	To          []string          `json:"to,omitempty"`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	Subject     string            `json:"subject"` // 只供 log 使用，最多 config.MetaSubjectLimit 字
	Labels      map[string]string `json:"labels,omitempty"`
	MessageID   string            `json:"message_id"`
	Attachments int               `json:"attachments"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Name 為佇列檔名所代表的排程資訊。
type Name struct {
	Due      time.Time // 下次寄送時間（秒）
	MailID   string
	Failures int // 已失敗次數；第一次寄送前為 0，檔名不帶這一段
}

func (n Name) String() string {
	s := strconv.FormatInt(n.Due.Unix(), 10) + "_" + n.MailID
	if n.Failures > 0 {
		s += "_" + strconv.Itoa(n.Failures)
	}
	return s + fileExt
}

// ParseName 解析佇列檔名；不符合格式時回傳 false。
func ParseName(filename string) (Name, bool) {
	base, ok := strings.CutSuffix(filename, fileExt)
	if !ok {
		return Name{}, false
	}
	parts := strings.Split(base, "_")
	if len(parts) != 2 && len(parts) != 3 {
		return Name{}, false
	}
	due, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || due < 0 || parts[1] == "" {
		return Name{}, false
	}
	n := Name{Due: time.Unix(due, 0), MailID: parts[1]}
	if len(parts) == 3 {
		failures, err := strconv.Atoi(parts[2])
		if err != nil || failures <= 0 {
			return Name{}, false
		}
		n.Failures = failures
	}
	return n, true
}

// Spool 為 spool_dir 底下的 tmp/（寫入中）與 queue/（等待寄送）兩個資料夾。
type Spool struct {
	tmpDir   string
	queueDir string
}

// Open 建立（或沿用）spool 資料夾，並清空 tmp/ 裡寫到一半就停掉留下的檔案。
func Open(dir string) (*Spool, error) {
	s := &Spool{tmpDir: filepath.Join(dir, "tmp"), queueDir: filepath.Join(dir, "queue")}
	for _, d := range []string{dir, s.tmpDir, s.queueDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create spool dir %q failed: %w", d, err)
		}
	}
	entries, err := os.ReadDir(s.tmpDir)
	if err != nil {
		return nil, fmt.Errorf("read spool tmp dir failed: %w", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(s.tmpDir, e.Name())); err != nil {
			return nil, fmt.Errorf("clean spool tmp dir failed: %w", err)
		}
	}
	return s, nil
}

// Write 寫入一封郵件：先寫到 tmp/ 並 fsync，再 rename 到 queue/。rename 是原子操作，
// queue/ 裡不會出現寫到一半的檔案。body 負責寫出 MIME 郵件內容。
func (s *Spool) Write(name Name, meta *Meta, body func(io.Writer) error) (err error) {
	line, err := encodeMeta(meta)
	if err != nil {
		return err
	}

	tmpPath := filepath.Join(s.tmpDir, name.String())
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create queue file failed: %w", err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmpPath)
		}
	}()

	bw := bufio.NewWriter(f)
	if _, err := bw.Write(line); err != nil {
		return fmt.Errorf("write queue file failed: %w", err)
	}
	if err := body(bw); err != nil {
		return fmt.Errorf("write queue file failed: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("write queue file failed: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync queue file failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close queue file failed: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(s.queueDir, name.String())); err != nil {
		return fmt.Errorf("move queue file failed: %w", err)
	}
	return syncDir(s.queueDir)
}

// encodeMeta 把 Meta 編成一行 JSON（含結尾換行）。Encoder 不跳脫 <、>、&，
// 字串裡的換行一律寫成 \n，輸出保證只有一行。
func encodeMeta(meta *Meta) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(meta); err != nil {
		return nil, fmt.Errorf("encode queue meta failed: %w", err)
	}
	if buf.Len() > config.MetaLineLimit {
		return nil, fmt.Errorf("queue meta line is %d bytes, exceeding the %d byte limit", buf.Len(), config.MetaLineLimit)
	}
	return buf.Bytes(), nil
}

// List 列出 queue/ 裡的佇列檔；檔名不符合格式的檔案放在 invalid 回傳。
func (s *Spool) List() (names []Name, invalid []string, err error) {
	entries, err := os.ReadDir(s.queueDir)
	if err != nil {
		return nil, nil, fmt.Errorf("read spool queue dir failed: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if n, ok := ParseName(e.Name()); ok {
			names = append(names, n)
		} else {
			invalid = append(invalid, e.Name())
		}
	}
	return names, invalid, nil
}

// Entry 為打開的佇列檔：Meta 已解析，Body 從第二行開始讀出 MIME 郵件內容。
type Entry struct {
	Meta Meta
	Body io.Reader
	// Size 為郵件內容（不含第一行）的大小。
	Size int64
	f    *os.File
}

func (e *Entry) Close() error { return e.f.Close() }

// OpenEntry 打開佇列檔並解析第一行。檔案不存在時回傳 os.ErrNotExist；
// 第一行無法解析時回傳 ErrCorrupted。
func (s *Spool) OpenEntry(name Name) (*Entry, error) {
	f, err := os.Open(filepath.Join(s.queueDir, name.String()))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// 緩衝區大小就是第一行的上限：讀到上限還沒遇到換行，就是損壞的檔案。
	// 之後的郵件內容沿用同一個 reader 串流讀出，緩衝區裡多讀的部分才不會遺失。
	r := bufio.NewReaderSize(f, config.MetaLineLimit)
	line, err := r.ReadSlice('\n')
	if err != nil {
		f.Close()
		if errors.Is(err, bufio.ErrBufferFull) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: meta line not found within %d bytes", ErrCorrupted, config.MetaLineLimit)
		}
		return nil, err
	}
	entry := &Entry{Body: r, Size: info.Size() - int64(len(line)), f: f}
	if err := json.Unmarshal(line, &entry.Meta); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if entry.Meta.MailID != name.MailID {
		f.Close()
		return nil, fmt.Errorf("%w: mail_id %q does not match file name", ErrCorrupted, entry.Meta.MailID)
	}
	return entry, nil
}

// Reschedule 依新的排程資訊改名（下次寄送時間、失敗次數），mail_id 不變。
func (s *Spool) Reschedule(old, next Name) error {
	if err := os.Rename(filepath.Join(s.queueDir, old.String()), filepath.Join(s.queueDir, next.String())); err != nil {
		return fmt.Errorf("reschedule queue file failed: %w", err)
	}
	return nil
}

// Remove 刪除佇列檔（寄出或判定失敗後）。
func (s *Spool) Remove(name Name) error {
	if err := os.Remove(filepath.Join(s.queueDir, name.String())); err != nil {
		return fmt.Errorf("remove queue file failed: %w", err)
	}
	return nil
}

// RemoveInvalid 刪除檔名不符合格式的檔案。
func (s *Spool) RemoveInvalid(filename string) error {
	return os.Remove(filepath.Join(s.queueDir, filename))
}

// syncDir fsync 資料夾，確保 rename 在當機後仍然存在。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open spool dir failed: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync spool dir failed: %w", err)
	}
	return nil
}
