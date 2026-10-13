package jsonstore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type item struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type doc struct {
	Title string `json:"title"`
	Items []item `json:"items"`
	Count int    `json:"count"`
}

func newDoc() doc {
	return doc{Title: "未命名"}
}

func TestOpenCreatesFileWithDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")

	s, err := Open(path, newDoc())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}

	// 初始值应当立刻落盘
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据文件没有被创建: %v", err)
	}
	s.View(func(d *doc) {
		if d.Title != "未命名" {
			t.Fatalf("初始值不对: %q", d.Title)
		}
	})
}

func TestUpdatePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")

	s, err := Open(path, newDoc())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	err = s.Update(func(d *doc) error {
		d.Title = "小卖部"
		d.Items = append(d.Items, item{ID: 1, Name: "马克杯"})
		return nil
	})
	if err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	again, err := Open(path, newDoc())
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	again.View(func(d *doc) {
		if d.Title != "小卖部" {
			t.Errorf("标题没存住: %q", d.Title)
		}
		if len(d.Items) != 1 || d.Items[0].Name != "马克杯" {
			t.Errorf("列表没存住: %+v", d.Items)
		}
	})
}

func TestUpdateErrorSkipsSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")

	s, err := Open(path, newDoc())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	want := fmt.Errorf("故意失败")
	if err := s.Update(func(d *doc) error {
		d.Title = "不该被保存"
		return want
	}); err != want {
		t.Fatalf("Update 应当原样返回回调的错误，得到 %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	var got doc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("解析文件失败: %v", err)
	}
	if got.Title != "未命名" {
		t.Errorf("回调报错时不应该落盘，文件里却是 %q", got.Title)
	}
}

func TestBrokenFileIsBackedUpAndRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")
	if err := os.WriteFile(path, []byte("{这不是合法 JSON"), 0o644); err != nil {
		t.Fatalf("准备坏文件失败: %v", err)
	}

	s, err := Open(path, newDoc())
	if err != nil {
		t.Fatalf("坏文件应该能自动重建，却报错: %v", err)
	}
	s.View(func(d *doc) {
		if d.Title != "未命名" {
			t.Errorf("重建后的内容不对: %q", d.Title)
		}
	})

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	backedUp := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".broken-") {
			backedUp = true
		}
	}
	if !backedUp {
		t.Error("坏文件应当被改名备份，却没找到备份文件")
	}
}

func TestConcurrentUpdatesKeepFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.json")

	s, err := Open(path, newDoc())
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}

	const workers, perWorker = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				if err := s.Update(func(d *doc) error {
					d.Count++
					d.Items = append(d.Items, item{ID: w*perWorker + i})
					return nil
				}); err != nil {
					t.Errorf("并发 Update 失败: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// 磁盘上必须是一份完整、可解析、计数正确的新数据
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	var got doc
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("并发写之后文件解析不了: %v", err)
	}
	if want := workers * perWorker; got.Count != want {
		t.Errorf("计数应当是 %d，实际 %d", want, got.Count)
	}
	if len(got.Items) != workers*perWorker {
		t.Errorf("列表长度应当是 %d，实际 %d", workers*perWorker, len(got.Items))
	}

	// 目录里不该留下临时文件
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("残留了临时文件: %s", e.Name())
		}
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open("", newDoc()); err == nil {
		t.Error("空路径应当报错")
	}
}
