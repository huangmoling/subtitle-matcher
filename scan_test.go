//go:build windows

package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// mkTree 按 "相对路径" 列表造文件（自动建父目录）
func mkTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, rel := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func names(ts []target) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	sort.Strings(out)
	return out
}

func TestScanVideosRecursive(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"SNOS-115/SNOS-115.strm",
		"剧集/ABF-353/ABF-353.mp4",
		"深/更/深/ZZZZ-999/ZZZZ-999.mkv",
	)

	ts, _, err := scanVideos(root, false)
	if err != nil {
		t.Fatal(err)
	}
	got := names(ts)
	want := []string{"ABF-353.mp4", "SNOS-115.strm", "ZZZZ-999.mkv"}
	if len(got) != len(want) {
		t.Fatalf("数量不对: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// .strm 是 Jellyfin/Emby 的流占位文件，必须和真实视频一样被认出来
func TestScanVideosAcceptsStrm(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root, "SNOS-115/SNOS-115.strm")

	ts, _, err := scanVideos(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("应该找到 1 个，实际 %d", len(ts))
	}
	if ts[0].Base != "SNOS-115" {
		t.Errorf("Base 应为 SNOS-115，实际 %q", ts[0].Base)
	}
	if ts[0].Rel != "SNOS-115" {
		t.Errorf("Rel 应为 SNOS-115，实际 %q", ts[0].Rel)
	}
}

// 字幕扩展名不能被当成视频
func TestScanVideosIgnoresNonVideo(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"SNOS-115/SNOS-115.strm",
		"SNOS-115/SNOS-115.srt",
		"SNOS-115/SNOS-115.nfo",
		"SNOS-115/poster.jpg",
		"SNOS-115/notes.txt",
	)

	ts, _, err := scanVideos(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Name != "SNOS-115.strm" {
		t.Fatalf("只应找到 SNOS-115.strm，实际 %v", names(ts))
	}
}

// 一个目录里多个视频时，每个都要单独处理
func TestScanVideosMultiplePerDir(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"SNOS-115/SNOS-115-cd1.mp4",
		"SNOS-115/SNOS-115-cd2.mp4",
	)

	ts, _, err := scanVideos(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("应找到 2 个，实际 %v", names(ts))
	}
}

func TestScanVideosOnlyCodes(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"SNOS-115/SNOS-115.strm",
		"电影/Some.Movie.2024.1080p/Some.Movie.2024.1080p.mkv",
	)

	ts, ignored, err := scanVideos(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Base != "SNOS-115" {
		t.Fatalf("只应保留 SNOS-115，实际 %v", names(ts))
	}
	if len(ignored) != 1 {
		t.Fatalf("应记录 1 个被忽略，实际 %v", ignored)
	}

	// 关掉过滤就该全部收下
	all, _, err := scanVideos(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("不过滤时应找到 2 个，实际 %v", names(all))
	}
}

// 同步软件/隐藏目录要跳过，不然会白扫一堆杂物
func TestScanVideosSkipsJunkDirs(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"SNOS-115/SNOS-115.strm",
		"@eaDir/SNOS-115/SNOS-115.mp4",
		".hidden/SNOS-149/SNOS-149.mp4",
	)

	ts, _, err := scanVideos(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Base != "SNOS-115" {
		t.Fatalf("应跳过 @eaDir 与隐藏目录，实际 %v", names(ts))
	}
}

// 关键回归：判断"是否已有字幕"必须按视频文件名，
// 不能只看目录里有没有字幕——否则同目录的其它视频会被一起误跳过。
func TestHasSubtitleFor(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root,
		"SNOS-115/SNOS-115.strm",
		"SNOS-115/SNOS-149.strm",
		"SNOS-115/SNOS-115.srt",
	)

	if _, ok := hasSubtitleFor(target{Dir: filepath.Join(root, "SNOS-115"), Base: "SNOS-115"}); !ok {
		t.Error("SNOS-115 已有同名字幕，应该命中")
	}
	if _, ok := hasSubtitleFor(target{Dir: filepath.Join(root, "SNOS-115"), Base: "SNOS-149"}); ok {
		t.Error("SNOS-149 没有自己的字幕，不该被同目录的 SNOS-115.srt 误判")
	}
}

func TestHasSubtitleForCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	mkTree(t, root, "SNOS-115/SNOS-115.strm", "SNOS-115/snos-115.SRT")

	if _, ok := hasSubtitleFor(target{Dir: filepath.Join(root, "SNOS-115"), Base: "SNOS-115"}); !ok {
		t.Error("大小写不同也应视为同名字幕")
	}
}

func TestNaturalLess(t *testing.T) {
	// 纯字符串比较会把 ABF-10 排在 ABF-2 前面，自然序不会
	in := []string{"ABF-10", "ABF-2", "ABF-1", "ABF-100", "SNOS-115"}
	sort.Slice(in, func(i, j int) bool { return naturalLess(in[i], in[j]) })
	want := []string{"ABF-1", "ABF-2", "ABF-10", "ABF-100", "SNOS-115"}
	for i := range want {
		if in[i] != want[i] {
			t.Fatalf("自然序不对: got %v want %v", in, want)
		}
	}
}

func TestNaturalLessPrefix(t *testing.T) {
	if !naturalLess("SNOS-115", "SNOS-115a") {
		t.Error("短前缀应排在长串前面")
	}
	if naturalLess("SNOS-115b", "SNOS-115a") {
		t.Error("b 不该排在 a 前面")
	}
}

func TestStatusRank(t *testing.T) {
	// 失败最该被看到，已完成最不需要
	if statusRank(stFailed.Text()) >= statusRank(stDone.Text()) {
		t.Error("失败应排在已完成前面")
	}
	if statusRank(stFailed.Text()) >= statusRank(stWaiting.Text()) {
		t.Error("失败应排在等待中前面")
	}
}
