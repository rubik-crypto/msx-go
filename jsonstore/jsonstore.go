// Package jsonstore 把一份 JSON 数据持久化到文件，供小体量的 Go 服务保存配置、
// 订单、留言这类「整体读写」的数据。
//
// 用法：内存里始终留一份解好的数据，读走 View、写走 Update，Update 会加写锁、
// 调用回调、再整体落盘；落盘用「先写临时文件再改名」，写一半失败也不会毁掉原文件。
//
//	type db struct {
//		Users  []User `json:"users"`
//		Config Config `json:"config"`
//	}
//
//	store, err := jsonstore.Open("data/db.json", db{Config: Config{Title: "未命名"}})
//	if err != nil {
//		log.Fatal(err)
//	}
//	store.Update(func(d *db) error {
//		d.Users = append(d.Users, User{Name: "张三"})
//		return nil
//	})
//	store.View(func(d *db) { fmt.Println(len(d.Users)) })
//
// 数据量小、改动不频繁时这份实现足够且不会写坏文件；数据一大就要换成真正的数据库。
package jsonstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store 一份带读写锁的 JSON 文档。
type Store[T any] struct {
	mu   sync.RWMutex
	path string
	v    T
}

// Open 从 path 读取数据，类型参数 T 就是数据的值类型（比如一个结构体）。
//
//   - 文件不存在：用 def 当初始值并立刻写入，方便第一次跑就有一份能用的配置。
//   - 文件内容解析不了：把坏文件改名成 <名字>.broken-<时间> 备份，再用 def 重建，
//     宁可丢一次数据也不要让整个服务起不来。
//   - path 为空会返回错误。
func Open[T any](path string, def T) (*Store[T], error) {
	if path == "" {
		return nil, errors.New("jsonstore: 文件路径不能为空")
	}

	s := &Store[T]{path: path}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var v T
		if jsonErr := json.Unmarshal(data, &v); jsonErr == nil {
			s.v = v
			return s, nil
		}
		backup := fmt.Sprintf("%s.broken-%s", path, time.Now().Format("20060102150405"))
		if renameErr := os.Rename(path, backup); renameErr != nil {
			return nil, fmt.Errorf("jsonstore: 备份损坏的文件 %s 失败: %w", path, renameErr)
		}
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("jsonstore: 读取 %s 失败: %w", path, err)
	}

	s.v = def
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 返回数据文件路径。
func (s *Store[T]) Path() string { return s.path }

// View 只读地访问数据。回调里不要再调用同一个 Store 的方法，否则会死锁；
// 也别把回调收到的指针存下来在锁外面用。
func (s *Store[T]) View(fn func(v *T)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.v)
}

// Update 加写锁修改数据，回调返回 nil 就整体落盘；回调返回错误则原样返回且不落盘。
func (s *Store[T]) Update(fn func(v *T) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.v); err != nil {
		return err
	}
	return s.saveLocked()
}

// Set 整个替换数据并落盘。
func (s *Store[T]) Set(v T) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.v = v
	return s.saveLocked()
}

// saveLocked 把当前数据写回文件，调用前必须已持有写锁。
func (s *Store[T]) saveLocked() error {
	data, err := json.MarshalIndent(s.v, "", "  ")
	if err != nil {
		return fmt.Errorf("jsonstore: 序列化数据失败: %w", err)
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("jsonstore: 创建目录 %s 失败: %w", dir, err)
	}

	// 先写同目录下的临时文件，落盘后再改名。改名是原子操作，
	// 所以任何时刻从磁盘读到的要么是旧内容、要么是完整的新内容。
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("jsonstore: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("jsonstore: 写入 %s 失败: %w", s.path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("jsonstore: 同步 %s 失败: %w", s.path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("jsonstore: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("jsonstore: 保存 %s 失败: %w", s.path, err)
	}
	return nil
}
