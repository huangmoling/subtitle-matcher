//go:build windows

package main

import (
	"testing"
	"time"
)

// withDefaults 是踩坑加上的：调用方只填了部分字段时，
// 漏掉的 SearchTimeout 会是 0，context.WithTimeout(ctx, 0) 立刻到期，
// 表现出来就是"三个站点全部响应超时"。
func TestConfigWithDefaults(t *testing.T) {
	// 只填了 Concurrency —— 曾经的真实调用方式
	got := config{Concurrency: 5}.withDefaults()

	if got.SearchTimeout <= 0 {
		t.Error("SearchTimeout 应被补上默认值，否则搜索会立刻超时")
	}
	if got.DownloadTimeout <= 0 {
		t.Error("DownloadTimeout 应被补上默认值")
	}
	if got.Concurrency != 5 {
		t.Errorf("显式设置的 Concurrency 不应被覆盖，got %d", got.Concurrency)
	}

	// 零值配置应等价于默认配置
	zero := config{}.withDefaults()
	def := defaultConfig()
	if zero.SearchTimeout != def.SearchTimeout || zero.DownloadTimeout != def.DownloadTimeout ||
		zero.Concurrency != def.Concurrency {
		t.Errorf("零值配置应等于默认配置，got %+v", zero)
	}

	// 默认值本身必须是正数
	if def.SearchTimeout <= 0 || def.DownloadTimeout <= 0 || def.Concurrency <= 0 {
		t.Errorf("默认配置含有非正值: %+v", def)
	}
	if def.SearchTimeout > 2*time.Minute {
		t.Errorf("搜索超时 %v 太长，卡住的站点会把任务拖死", def.SearchTimeout)
	}
}
